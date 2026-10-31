package gatekeeper

import (
	"compress/zlib"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shaoxier/AIofCage/internal/audit"
)

const (
	connectorPlaceholder = "cage-placeholder-connector-pg-1"
	connectorToken       = "synthetic-connector-token-0123456789abcdef"
)

type mcpFixture struct {
	*fixture
	upstreamCalls atomic.Int32
	lastAuth      atomic.Value
	status        atomic.Int32
}

func newMCPFixture(t *testing.T) *mcpFixture {
	t.Helper()
	f := &mcpFixture{fixture: newFixture(t, true)}
	f.status.Store(200)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.upstreamCalls.Add(1)
		f.lastAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "session-from-connector")
		w.WriteHeader(int(f.status.Load()))
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID,
			"result": map[string]string{"method": msg.Method, "session": r.Header.Get("Mcp-Session-Id")}})
	}))
	t.Cleanup(upstream.Close)
	f.gate.mcpClient.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "10.9.0.3:8080" {
			t.Errorf("unexpected connector address %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	config := testConfig(1)
	config.Connectors = []Connector{{ID: "pg", Placeholder: connectorPlaceholder, Upstream: "http://10.9.0.3:8080",
		Token: connectorToken, Tools: []string{"query"}}}
	f.configure(t, config)
	return f
}

func (f *mcpFixture) call(t *testing.T, path, placeholder, session, body string) (int, string, http.Header) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if placeholder != "" {
		r.Header.Set("Authorization", "Bearer "+placeholder)
	}
	if session != "" {
		r.Header.Set("Mcp-Session-Id", session)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.handlers.MCP.ServeHTTP(w, r)
	return w.Code, w.Body.String(), w.Header()
}

func TestMCPGatewayForwardsWithConnectorToken(t *testing.T) {
	f := newMCPFixture(t)
	seq := f.gate.events.last()
	status, body, header := f.call(t, "/mcp/pg", connectorPlaceholder, "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if status != 200 || !strings.Contains(body, `"method":"initialize"`) || header.Get("Mcp-Session-Id") != "session-from-connector" {
		t.Fatalf("initialize: %d %s %v", status, body, header)
	}
	if auth := f.lastAuth.Load(); auth != "Bearer "+connectorToken {
		t.Fatalf("connector must receive its own token, got %v", auth)
	}
	_, body, _ = f.call(t, "/mcp/pg", connectorPlaceholder, "session-from-connector",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"query","arguments":{"sql":"select 1"}}}`)
	if !strings.Contains(body, `"session":"session-from-connector"`) {
		t.Fatalf("session header must reach the connector: %s", body)
	}
	f.call(t, "/mcp/pg", connectorPlaceholder, "session-from-connector", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	f.call(t, "/mcp/pg", connectorPlaceholder, "session-from-connector", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if status, body, _ := f.call(t, "/mcp/pg", connectorPlaceholder, "", `{"jsonrpc":"2.0","id":4,"method":"server/discover"}`); status != 200 ||
		!strings.Contains(body, `"code":-32601`) {
		t.Fatalf("server/discover must look like an unsupported method: %d %s", status, body)
	}
	events := f.eventsAfter(t, seq)
	var calls []audit.Event
	for _, e := range events {
		if e.Action == audit.ActionMCPCall {
			calls = append(calls, e)
		}
	}
	// tools/list and notifications are forwarded but not recorded; probes are answered locally.
	if len(calls) != 2 || calls[0].Method != "initialize" || calls[1].ToolID != "query" || calls[1].ServiceID != "pg" ||
		calls[1].Decision != audit.DecisionAllow || calls[1].ReasonCode != "mcp_forwarded" {
		t.Fatalf("unexpected events: %+v", calls)
	}
	if strings.Contains(string(mustJSON(events)), "select 1") {
		t.Fatal("tool arguments must never be recorded")
	}
	if f.upstreamCalls.Load() != 4 {
		t.Fatalf("upstream calls: %d", f.upstreamCalls.Load())
	}
}

func TestMCPGatewayDenials(t *testing.T) {
	f := newMCPFixture(t)
	cases := []struct {
		name, path, placeholder, body string
		status                        int
		reason                        string
	}{
		{"unknown service", "/mcp/other", connectorPlaceholder, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, 404, "mcp_unknown_service"},
		{"no credential", "/mcp/pg", "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, 403, "credential_missing"},
		{"provider placeholder", "/mcp/pg", placeholderA, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, 403, "credential_mismatch"},
		{"invalid json", "/mcp/pg", connectorPlaceholder, `not json`, 400, "mcp_invalid_message"},
		{"tool not allowed", "/mcp/pg", connectorPlaceholder, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"drop_table"}}`, 200, "mcp_tool_denied"},
		{"method not allowed", "/mcp/pg", connectorPlaceholder, `{"jsonrpc":"2.0","id":9,"method":"sampling/createMessage"}`, 200, "mcp_method_denied"},
	}
	for _, c := range cases {
		seq := f.gate.events.last()
		status, body, _ := f.call(t, c.path, c.placeholder, "", c.body)
		if status != c.status {
			t.Errorf("%s: status %d %s", c.name, status, body)
			continue
		}
		if status == 200 && !strings.Contains(body, `"code":-32001`) {
			t.Errorf("%s: expected JSON-RPC error, got %s", c.name, body)
		}
		events := f.eventsAfter(t, seq)
		if last := events[len(events)-1]; last.Decision != audit.DecisionDeny || last.ReasonCode != c.reason {
			t.Errorf("%s: event %+v", c.name, last)
		}
	}
	if f.upstreamCalls.Load() != 0 {
		t.Fatalf("denied messages must not reach the connector: %d", f.upstreamCalls.Load())
	}
	f.status.Store(401)
	status, body, header := f.call(t, "/mcp/pg", connectorPlaceholder, "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if status == 401 || header.Get("WWW-Authenticate") != "" || !strings.Contains(body, `"code":-32001`) {
		t.Fatalf("connector auth failure must not surface as an auth challenge: %d %v %s", status, header, body)
	}
	if status, _, _ := f.call(t, "/mcp/pg", connectorPlaceholder, "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); status != 502 {
		t.Fatalf("a notification has no ID to answer, so the failure is a 502: %d", status)
	}
}

// Go reads object keys case-insensitively and keeps the last repeat; the connector's server reads
// them exactly. Messages where the two could disagree never reach the connector.
func TestMCPGatewayRefusesAmbiguousMessages(t *testing.T) {
	f := newMCPFixture(t)
	for name, body := range map[string]string{
		"tool name in two cases":  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"drop_table","Name":"query"}}`,
		"tool name in other case": `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"Name":"query"}}`,
		"repeated tool name":      `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","name":"drop_table"}}`,
		"method in two cases":     `{"jsonrpc":"2.0","id":1,"method":"tools/list","Method":"tools/call","params":{"name":"drop_table"}}`,
		"params folded":           `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query"},"paramſ":{"name":"drop_table"}}`,
		"repeated method":         `{"jsonrpc":"2.0","id":1,"method":"ping","method":"tools/call","params":{"name":"drop_table"}}`,
		"batch":                   `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"drop_table"}}]`,
		"second value":            `{"jsonrpc":"2.0","id":1,"method":"ping"}{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"drop_table"}}`,
		"tool name not a string":  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":["query"]}}`,
		"invalid utf-8":           "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"query\xff\"}}",
	} {
		seq := f.gate.events.last()
		status, response, _ := f.call(t, "/mcp/pg", connectorPlaceholder, "", body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, status, response)
			continue
		}
		if events := f.eventsAfter(t, seq); events[len(events)-1].ReasonCode != "mcp_invalid_message" {
			t.Errorf("%s: %+v", name, events[len(events)-1])
		}
	}
	if f.upstreamCalls.Load() != 0 {
		t.Fatalf("ambiguous messages reached the connector: %d", f.upstreamCalls.Load())
	}
}

func TestMCPGatewayListsOnlyGrantedTools(t *testing.T) {
	f := newMCPFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"query","inputSchema":{}},{"name":"drop_table","inputSchema":{}}],"nextCursor":"c2"}}`)
	}))
	t.Cleanup(upstream.Close)
	f.gate.mcpClient.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	status, body, _ := f.call(t, "/mcp/pg", connectorPlaceholder, "", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if status != 200 || !strings.Contains(body, `"query"`) || strings.Contains(body, "drop_table") || !strings.Contains(body, `"nextCursor":"c2"`) {
		t.Fatalf("tools/list must keep only granted tools: %d %s", status, body)
	}
}

func TestMCPGatewayExplicitEmptyToolGrantDeniesEveryTool(t *testing.T) {
	f := newMCPFixture(t)
	config := testConfig(2)
	config.Connectors = []Connector{{ID: "pg", Placeholder: connectorPlaceholder, Upstream: "http://10.9.0.3:8080",
		Token: connectorToken, Tools: []string{}, ToolsRestricted: true}}
	f.configure(t, config)
	seq := f.gate.events.last()
	status, body, _ := f.call(t, "/mcp/pg", connectorPlaceholder, "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query"}}`)
	if status != http.StatusOK || !strings.Contains(body, `"code":-32001`) {
		t.Fatalf("explicit empty tool grant must deny the call: %d %s", status, body)
	}
	if f.upstreamCalls.Load() != 0 {
		t.Fatalf("denied tool call reached the connector: %d", f.upstreamCalls.Load())
	}
	events := f.eventsAfter(t, seq)
	if len(events) == 0 || events[len(events)-1].ReasonCode != "mcp_tool_denied" || events[len(events)-1].Decision != audit.DecisionDeny {
		t.Fatalf("missing denial audit event: %+v", events)
	}
}

func TestConnectorConfigValidation(t *testing.T) {
	valid := Connector{ID: "pg", Placeholder: connectorPlaceholder, Upstream: "http://10.9.0.3:8080", Token: connectorToken}
	cases := map[string]func(*Connector){
		"public upstream":       func(c *Connector) { c.Upstream = "http://8.8.8.8:80" },
		"https upstream":        func(c *Connector) { c.Upstream = "https://10.9.0.3:8080" },
		"hostname upstream":     func(c *Connector) { c.Upstream = "http://connector:8080" },
		"loopback upstream":     func(c *Connector) { c.Upstream = "http://127.0.0.1:8080" },
		"upstream with path":    func(c *Connector) { c.Upstream = "http://10.9.0.3:8080/x" },
		"short token":           func(c *Connector) { c.Token = "short" },
		"bad id":                func(c *Connector) { c.ID = "Bad ID" },
		"placeholder of secret": func(c *Connector) { c.Placeholder = placeholderA },
	}
	for name, mutate := range cases {
		connector := valid
		mutate(&connector)
		config := testConfig(1)
		config.Connectors = []Connector{connector}
		if _, err := compile(config); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	config := testConfig(1)
	config.Connectors = []Connector{valid}
	if _, err := compile(config); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

// An agent that sends the session close while exiting hangs up at once. The connector must still
// end the session, and neither that nor a repeated close is a connector failure.
func TestMCPClientHangUpIsNotAnUpstreamFailure(t *testing.T) {
	f := newMCPFixture(t)
	seq := f.gate.events.last()
	send := func(method, body string, cancelled bool) int {
		r := httptest.NewRequest(method, "/mcp/pg", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+connectorPlaceholder)
		r.Header.Set("Mcp-Session-Id", "session-from-connector")
		if cancelled {
			ctx, cancel := context.WithCancel(r.Context())
			cancel()
			r = r.WithContext(ctx)
		}
		w := httptest.NewRecorder()
		f.handlers.MCP.ServeHTTP(w, r)
		return w.Code
	}
	send(http.MethodDelete, "", true)
	if f.upstreamCalls.Load() != 1 {
		t.Fatalf("a close from a client that hung up must still reach the connector: %d", f.upstreamCalls.Load())
	}
	f.status.Store(404)
	if status := send(http.MethodDelete, "", false); status != 404 {
		t.Fatalf("repeated close: %d", status)
	}
	f.status.Store(200)
	send(http.MethodPost, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"query"}}`, true)
	events, _, _ := f.gate.events.since(seq)
	var calls []audit.Event
	for _, e := range events {
		if e.Decision == audit.DecisionError {
			t.Errorf("hang-ups and repeated closes are not errors: %+v", e)
		}
		if e.Action == audit.ActionMCPCall {
			calls = append(calls, e)
		}
	}
	if len(calls) != 1 || calls[0].ReasonCode != "client_closed" || calls[0].ToolID != "query" {
		t.Fatalf("a tool call abandoned by the agent is recorded as client_closed: %+v", calls)
	}
}

const (
	remotePlaceholder = "cage-placeholder-remote-docs-1"
	remoteToken       = "synthetic-remote-mcp-token-0123456789abcdef"
)

// newRemoteFixture serves a remote MCP service at https://model-a.example/mcp with verified TLS.
func newRemoteFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, true)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer "+remoteToken {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://model-a.example/.well-known/oauth-protected-resource"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Header.Get("Mcp-Session-Id") {
		case "expired":
			http.Error(w, "token expired", http.StatusUnauthorized)
			return
		case "moved":
			http.Redirect(w, r, "https://elsewhere.example/mcp", http.StatusFound)
			return
		case "broken":
			http.Error(w, "bad request for "+remoteToken, http.StatusBadRequest)
			return
		case "deflated":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "deflate")
			compressed := zlib.NewWriter(w)
			io.WriteString(compressed, `{"jsonrpc":"2.0","id":9,"result":{"content":[{"type":"text","text":"echo `+remoteToken+`"}]}}`)
			compressed.Close()
			return
		case "empty-gzip":
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusAccepted)
			return
		case "brotli":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "br")
			io.WriteString(w, "not decodable")
			return
		}
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		w.Header().Set("Mcp-Session-Id", "remote-session")
		switch msg.Method {
		case "initialize":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, ": ping\n\nevent: message\r\nid: e1\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":"+string(msg.ID)+
				",\"result\":{\"protocolVersion\":\"2025-06-18\",\"capabilities\":{\"tools\":{\"listChanged\":true},\"resources\":{},\"prompts\":{}},"+
				"\"serverInfo\":{\"name\":\"docs\",\"version\":\"1\"}}}\r\n\r\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\r\ndata: \"params\":{}}\n\ndata: {\"cut\":")
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(msg.ID)+`,"result":{"tools":[`+
				`{"name":"search_docs","annotations":{"readOnlyHint":true},"inputSchema":{}},{"name":"delete_page","inputSchema":{}}]}}`)
		case "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, ": "+remoteToken+"\n\nid: "+remoteToken+"\ndata: {\"jsonrpc\":\"2.0\",\"id\":"+string(msg.ID)+
				",\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"<b>echo "+remoteToken+"</b>\"}]}}\n\n")
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})
	config := testConfig(1)
	config.Connectors = []Connector{{ID: "docs", Placeholder: remotePlaceholder, Upstream: "https://model-a.example/mcp",
		Token: remoteToken, Tools: []string{"search_docs"}, Remote: true}}
	f.configure(t, config)
	return f
}

func (f *fixture) callMCP(t *testing.T, session, body string) (int, string, http.Header) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp/docs", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+remotePlaceholder)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		r.Header.Set("Mcp-Session-Id", session)
	}
	w := httptest.NewRecorder()
	f.handlers.MCP.ServeHTTP(w, r)
	return w.Code, w.Body.String(), w.Header()
}

func TestRemoteMCPGatewayChecksToolsAndHoldsTheToken(t *testing.T) {
	f := newRemoteFixture(t)
	seq := f.gate.events.last()
	status, body, header := f.callMCP(t, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if status != 200 || header.Get("Mcp-Session-Id") != "remote-session" || !strings.Contains(body, ": ping\n\n") ||
		!strings.Contains(body, "event: message\nid: e1\ndata: {") || !strings.Contains(body, `"tools":{"listChanged":true}`) ||
		strings.Contains(body, `"resources"`) || strings.Contains(body, `"prompts"`) {
		t.Fatalf("initialize must arrive with only the tools capability: %d %s", status, body)
	}
	data := eventData(t, body)
	if len(data) != 2 || !strings.Contains(data[1], `"method":"notifications/message"`) {
		t.Fatalf("each complete event must arrive as valid JSON, and a cut-off one not at all: %q", data)
	}
	_, body, _ = f.callMCP(t, "remote-session", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if !strings.Contains(body, `"search_docs"`) || strings.Contains(body, "delete_page") {
		t.Fatalf("tools/list must keep only granted tools: %s", body)
	}
	_, body, _ = f.callMCP(t, "remote-session", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_docs","arguments":{"q":"private words"}}}`)
	if strings.Contains(body, remoteToken) || !strings.Contains(body, "[REDACTED]") || !strings.Contains(body, "<b>echo") {
		t.Fatalf("the service token must never reach the agent: %s", body)
	}
	before := f.calls.Load()
	_, body, _ = f.callMCP(t, "remote-session", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"delete_page"}}`)
	if !strings.Contains(body, `"code":-32001`) || !strings.Contains(body, "search_docs") {
		t.Fatalf("an ungranted tool must be refused with the granted ones named: %s", body)
	}
	_, body, _ = f.callMCP(t, "remote-session", `{"jsonrpc":"2.0","id":5,"method":"resources/list"}`)
	if body != "{\"id\":5,\"jsonrpc\":\"2.0\",\"result\":{\"resources\":[]}}\n" {
		t.Fatalf("resource listings are answered as empty by the gateway: %s", body)
	}
	_, body, _ = f.callMCP(t, "remote-session", `{"jsonrpc":"2.0","id":6,"method":"resources/read","params":{"uri":"repo://secret"}}`)
	if !strings.Contains(body, `"code":-32001`) {
		t.Fatalf("reading resources is not covered by a tool grant: %s", body)
	}
	if f.calls.Load() != before {
		t.Fatalf("refused and locally answered messages must not reach the service: %d", f.calls.Load()-before)
	}
	events := f.eventsAfter(t, seq)
	var recorded []string
	for _, e := range events {
		if e.Action != audit.ActionMCPCall {
			continue
		}
		if e.Host != "model-a.example" || e.ServiceID != "docs" {
			t.Errorf("remote calls are recorded with service and host: %+v", e)
		}
		recorded = append(recorded, e.Decision+":"+e.Method+":"+e.ToolID)
	}
	want := "allow:initialize:,allow:tools/call:search_docs,deny:tools/call:delete_page,deny:resources/read:"
	if strings.Join(recorded, ",") != want {
		t.Fatalf("recorded %v, want %s", recorded, want)
	}
	if data := string(mustJSON(events)); strings.Contains(data, "private words") || strings.Contains(data, remoteToken) {
		t.Fatal("arguments and the token must never be recorded")
	}
}

func TestRemoteMCPGatewayFailures(t *testing.T) {
	f := newRemoteFixture(t)
	status, body, header := f.callMCP(t, "expired", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if status != 200 || header.Get("WWW-Authenticate") != "" || !strings.Contains(body, "重新登录") {
		t.Fatalf("a rejected service token must not become an auth challenge for the agent: %d %v %s", status, header, body)
	}
	status, body, _ = f.callMCP(t, "moved", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if status != 200 || strings.Contains(body, "elsewhere.example") || !strings.Contains(body, "跳转") {
		t.Fatalf("redirects are neither followed nor revealed: %d %s", status, body)
	}
	status, body, _ = f.callMCP(t, "broken", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if status != 400 || strings.Contains(body, remoteToken) || !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("error bodies are relayed with the token masked: %d %s", status, body)
	}
	status, body, header = f.callMCP(t, "deflated", `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"search_docs"}}`)
	if status != 200 || header.Get("Content-Encoding") != "" || strings.Contains(body, remoteToken) || !strings.Contains(body, "echo [REDACTED]") {
		t.Fatalf("a compressed answer is decoded before masking and relayed decoded: %d %v %q", status, header, body)
	}
	status, body, _ = f.callMCP(t, "brotli", `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"search_docs"}}`)
	if status != 200 || !strings.Contains(body, `"code":-32001`) || strings.Contains(body, "not decodable") {
		t.Fatalf("an encoding the gateway cannot check is not relayed: %d %q", status, body)
	}
	status, _, _ = f.callMCP(t, "remote-session", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if status != http.StatusAccepted {
		t.Fatalf("notifications are relayed: %d", status)
	}
	if status, body, _ := f.callMCP(t, "empty-gzip", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); status != http.StatusAccepted {
		t.Fatalf("an empty answer declaring an encoding is still relayed: %d %s", status, body)
	}
	r := httptest.NewRequest(http.MethodGet, "/mcp/docs", nil)
	r.Header.Set("Authorization", "Bearer "+remotePlaceholder)
	w := httptest.NewRecorder()
	f.handlers.MCP.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("the gateway offers no server-initiated stream: %d", w.Code)
	}
}

func TestRemoteConnectorConfigValidation(t *testing.T) {
	valid := Connector{ID: "docs", Placeholder: remotePlaceholder, Upstream: "https://mcp.example.com/mcp", Token: remoteToken, Remote: true}
	cases := map[string]func(*Connector){
		"plain http":      func(c *Connector) { c.Upstream = "http://mcp.example.com/mcp" },
		"ip address":      func(c *Connector) { c.Upstream = "https://8.8.8.8/mcp" },
		"query":           func(c *Connector) { c.Upstream = "https://mcp.example.com/mcp?key=1" },
		"other port":      func(c *Connector) { c.Upstream = "https://mcp.example.com:8443/mcp" },
		"credentials":     func(c *Connector) { c.Upstream = "https://user:pass@mcp.example.com/mcp" },
		"unclean path":    func(c *Connector) { c.Upstream = "https://mcp.example.com/a/../mcp" },
		"uppercase host":  func(c *Connector) { c.Upstream = "https://MCP.example.com/mcp" },
		"token spaces":    func(c *Connector) { c.Token = "two words" },
		"placeholder tok": func(c *Connector) { c.Token = "cage-abcdefghijklmnop" },
		"short token":     func(c *Connector) { c.Token = "id" },
		"control in tool": func(c *Connector) { c.Tools = []string{"read\nwrite"} },
	}
	for name, mutate := range cases {
		connector := valid
		mutate(&connector)
		config := testConfig(1)
		config.Connectors = []Connector{connector}
		if _, err := compile(config); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	config := testConfig(1)
	open := valid
	open.Token = "" // a public service needs no token
	config.Connectors = []Connector{open}
	compiled, err := compile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !compiled.connectors["docs"].ToolsRestricted || compiled.connectors["docs"].allows("anything") {
		t.Fatal("a remote service without granted tools must deny every tool")
	}
}

// eventData parses an event stream the way a client does (lines end at CRLF, LF or CR) and
// checks each event's data is JSON.
func eventData(t *testing.T, stream string) []string {
	t.Helper()
	stream = strings.ReplaceAll(strings.ReplaceAll(stream, "\r\n", "\n"), "\r", "\n")
	var result []string
	for _, block := range strings.Split(stream, "\n\n") {
		var lines []string
		for _, line := range strings.Split(block, "\n") {
			if value, ok := strings.CutPrefix(line, "data:"); ok {
				lines = append(lines, strings.TrimPrefix(value, " "))
			} else if line != "" && !strings.HasPrefix(line, ":") && !strings.HasPrefix(line, "event:") && !strings.HasPrefix(line, "id:") {
				t.Fatalf("unexpected line %q in %q", line, stream)
			}
		}
		if len(lines) == 0 {
			continue
		}
		data := strings.Join(lines, "\n")
		if !json.Valid([]byte(data)) {
			t.Fatalf("event data is not JSON: %q", data)
		}
		result = append(result, data)
	}
	return result
}
