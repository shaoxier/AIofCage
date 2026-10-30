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
		"escaped key":  {`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"query"}}`, "tools/call", "query"},
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
		"escaped repeat":     `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","name":"drop"}}`,
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
