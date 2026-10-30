package gatekeeper

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// The premise of the strict parser: Go's decoder folds key case (with Unicode folding) and keeps
// the last repeat, so a struct-based check reads another tool than an exact-matching server.
func TestGoDecoderFoldsKeysTheGatewayRefuses(t *testing.T) {
	var loose struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	body := `{"method":"tools/call","params":{"name":"delete_page"},"paramſ":{"name":"search_docs"}}`
	if err := json.Unmarshal([]byte(body), &loose); err != nil || loose.Params.Name != "search_docs" {
		t.Fatalf("expected Go to read the folded key: %+v %v", loose, err)
	}
	if _, err := parseMCPMessage([]byte(body)); err == nil {
		t.Fatal("the gateway must refuse a message Go and the server read differently")
	}
	if foldKey("K") != foldKey("k") || foldKey("ſ") != foldKey("S") || foldKey("name") == foldKey("names") {
		t.Fatal("foldKey must follow simple case folding")
	}
}

func TestParseMCPMessageAcceptsOrdinaryMessages(t *testing.T) {
	cases := map[string]struct{ body, method, tool string }{
		"request":      {`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","Capabilities":{}}}`, "initialize", ""},
		"notification": {`{"jsonrpc":"2.0","method":"notifications/initialized"}`, "notifications/initialized", ""},
		"tool call":    {`{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"query","arguments":{"Name":"x","name":"y"},"_meta":{"progressToken":1}}}`, "tools/call", "query"},
		"escaped key":  {`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"n\u0061me":"query"}}`, "tools/call", "query"},
		"client reply": {` {"jsonrpc":"2.0","id":7,"result":{"roots":[]}} `, "", ""},
	}
	for name, c := range cases {
		msg, err := parseMCPMessage([]byte(c.body))
		if err != nil || msg.Method != c.method || msg.Tool != c.tool {
			t.Errorf("%s: %+v %v", name, msg, err)
		}
	}
	for name, body := range map[string]string{
		"reply without id":   `{"jsonrpc":"2.0","result":{}}`,
		"reply with both":    `{"jsonrpc":"2.0","id":1,"result":{},"error":{}}`,
		"empty method":       `{"jsonrpc":"2.0","id":1,"method":""}`,
		"call without name":  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`,
		"call params array":  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":["query"]}`,
		"too many keys":      `{` + manyKeys(70) + `"method":"ping"}`,
		"escaped repeat":     `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","n\u0061me":"drop"}}`,
		"escaped case":       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"N\u0061me":"query"}}`,
		"not an object":      `"tools/call"`,
		"unterminated":       `{"jsonrpc":"2.0","id":1,"method":"ping"`,
		"method is a number": `{"jsonrpc":"2.0","id":1,"method":7}`,
	} {
		if _, err := parseMCPMessage([]byte(body)); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
}

func manyKeys(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(`"k` + strings.Repeat("x", i) + `":1,`)
	}
	return b.String()
}

func TestCopyEventsKeepsStreamingShape(t *testing.T) {
	w := httptest.NewRecorder()
	stream := ": open\n\nretry: 1000\n\nid: 1\ndata: {\"a\":1}\n\n:inner\nevent: message\ndata: {\"b\":\ndata: 2}\n\n"
	n, err := copyEvents(w, strings.NewReader(stream), func(data []byte) ([]byte, error) { return data, nil }, nil)
	want := ": open\n\nretry: 1000\n\nid: 1\ndata: {\"a\":1}\n\n:inner\nevent: message\ndata: {\"b\":\ndata: 2}\n\n"
	if err != nil || w.Body.String() != want || n != int64(len(want)) {
		t.Fatalf("copyEvents changed the stream: %v %q", err, w.Body.String())
	}
	w = httptest.NewRecorder()
	_, err = copyEvents(w, strings.NewReader("data: {}\n\ndata: not json\n\n"), func(data []byte) ([]byte, error) {
		return mcpReply{method: "tools/list"}.message(data)
	}, nil)
	if err != errMCPReply || w.Body.String() != "data: {}\n\n" {
		t.Fatalf("an event that cannot be checked stops the stream before it is relayed: %v %q", err, w.Body.String())
	}
}

// Clients end a line at CRLF, LF or a lone CR and drop a byte order mark that opens the stream;
// the gateway must see the same events, or a filtered listing could slip past it.
func TestCopyEventsSplitsLinesLikeClients(t *testing.T) {
	listing := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search"},{"name":"delete_page"}]}}`
	reply := mcpReply{method: "tools/list", connector: Connector{Tools: []string{"search"}, ToolsRestricted: true}}
	for name, stream := range map[string]string{
		"lone CR":         "id: 1\rdata: " + listing + "\r\r",
		"CRLF":            "id: 1\r\ndata: " + listing + "\r\n\r\n",
		"CR then CRLF":    "data: " + listing + "\r\r\n",
		"byte order mark": "\xef\xbb\xbfdata: " + listing + "\n\n",
	} {
		w := httptest.NewRecorder()
		if _, err := copyEvents(w, strings.NewReader(stream), reply.message, nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out := w.Body.String()
		if !strings.Contains(out, "data: {") || !strings.Contains(out, `"search"`) || strings.Contains(out, "delete_page") {
			t.Errorf("%s: the listing must arrive filtered: %q", name, out)
		}
	}
	w := httptest.NewRecorder()
	n, err := copyEvents(w, strings.NewReader("data: {}\r\rdata: {}\r\r"), func(data []byte) ([]byte, error) { return data, nil }, nil)
	if err != nil || w.Body.String() != "data: {}\n\ndata: {}\n\n" || n != 20 {
		t.Fatalf("two CR-terminated events must stay two events: %q %v", w.Body.String(), err)
	}
}

func TestCopyEventsBoundsEveryLine(t *testing.T) {
	stream := "x: " + strings.Repeat("a", maxMCPReply) + "\n\n"
	if _, err := copyEvents(httptest.NewRecorder(), strings.NewReader(stream), func(data []byte) ([]byte, error) { return data, nil }, nil); err != errMCPReply {
		t.Fatalf("a field line without data must be bounded too: %v", err)
	}
}

// A token written with JSON escapes is what the agent's parser reads, so such a reply is withheld.
func TestRemoteRepliesWithEscapedTokensAreWithheld(t *testing.T) {
	secret := "gsynthetic_TokenValue0123456789"
	reply := mcpReply{method: "tools/call", connector: Connector{Remote: true, Token: secret}}
	for name, data := range map[string]string{
		"escaped":         `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"\u0067synthetic\u005fTokenValue0123456789"}]}}`,
		"escaped key":     `{"jsonrpc":"2.0","id":1,"result":{"\u0067synthetic_TokenValue0123456789":1}}`,
		"byte order mark": "\xef\xbb\xbf" + `{"jsonrpc":"2.0","id":1,"result":{"text":"\u0067synthetic_TokenValue0123456789"}}`,
	} {
		if _, err := reply.message([]byte(data)); err != errTokenInReply {
			t.Errorf("%s: %v", name, err)
		}
	}
	plain := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"\u003cb\u003e line\nbreak"}],"n":1.50}}`)
	if out, err := reply.message(plain); err != nil || string(out) != string(plain) {
		t.Fatalf("a message without the token is relayed byte for byte: %s %v", out, err)
	}
	exact := []byte(`{"jsonrpc":"2.0","id":1,"result":{"text":"gsynthetic_TokenValue0123456789"}}`)
	if out, err := reply.message(exact); err != nil || strings.Contains(string(out), secret) {
		t.Fatalf("a token written plainly is masked: %s %v", out, err)
	}
}

// Clients ignore unknown fields; relaying them could let a client that strips one more byte
// order mark read a line the gateway took for something else.
func TestCopyEventsDropsUnknownFields(t *testing.T) {
	listing := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"delete_page"}]}}`
	reply := mcpReply{method: "tools/list", connector: Connector{Tools: []string{"search"}, ToolsRestricted: true}}
	w := httptest.NewRecorder()
	stream := "\xef\xbb\xbf\xef\xbb\xbfdata: " + listing + "\n\nid: 7\nretry: 10\nevent: message\nfoo: bar\ndata: {}\n\n"
	if _, err := copyEvents(w, strings.NewReader(stream), reply.message, nil); err != nil {
		t.Fatal(err)
	}
	if out := w.Body.String(); strings.Contains(out, "delete_page") || strings.Contains(out, "foo") || !strings.Contains(out, "id: 7\nretry: 10\nevent: message\ndata: {}") {
		t.Fatalf("unknown fields must be dropped, known ones kept: %q", out)
	}
}
