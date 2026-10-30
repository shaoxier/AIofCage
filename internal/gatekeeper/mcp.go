package gatekeeper

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shaoxier/AIofCage/internal/audit"
)

// The MCP gateway is the only way from the sandbox to MCP services that need a secret: trusted
// connector containers (e.g. a database) and remote streamable-HTTP services, whose token the
// gatekeeper adds itself. Every message is checked against the service placeholder and the
// granted tools; tool calls are recorded by name only, never with arguments or results.

const (
	maxMCPBody     = 4 << 20
	mcpCallTimeout = 10 * time.Minute
)

var mcpMethods = map[string]bool{"initialize": true, "ping": true, "tools/list": true, "tools/call": true,
	"resources/list": true, "resources/read": true, "resources/templates/list": true, "prompts/list": true,
	"prompts/get": true, "completion/complete": true, "logging/setLevel": true}

// A remote service is granted tools only. Its resource and prompt listings are answered here as
// empty and reading a resource or prompt is refused, because no tool grant covers them.
var (
	remoteMethods    = map[string]bool{"initialize": true, "ping": true, "tools/list": true, "tools/call": true, "logging/setLevel": true}
	remoteEmptyLists = map[string]string{"resources/list": "resources", "resources/templates/list": "resourceTemplates", "prompts/list": "prompts"}
)

// Newer protocol probes that clients fall back from (Claude Code tries server/discover before
// initialize). They are answered like a server without them and not logged as denials.
var mcpProbes = map[string]bool{"server/discover": true}

// Handshakes and listings would only add noise to the request log; data access is recorded.
var mcpRecorded = map[string]bool{"initialize": true, "tools/call": true, "resources/read": true, "prompts/get": true}

var (
	mcpMethodPattern = regexp.MustCompile(`^[a-z][a-zA-Z/_]{0,63}$`)
	toolPattern      = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,128}$`)
)

var mcpForwardHeaders = []string{"Accept", "Mcp-Session-Id", "Mcp-Protocol-Version", "Last-Event-Id"}

func newMCPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			ResponseHeaderTimeout: mcpCallTimeout, IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 4},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (g *Gate) serveMCP(w http.ResponseWriter, r *http.Request) {
	started := g.now()
	active := g.current()
	if active == nil {
		writeProblem(w, http.StatusServiceUnavailable, "not_configured")
		return
	}
	event := audit.Event{Action: audit.ActionMCPCall, Decision: audit.DecisionDeny, Method: r.Method,
		SessionID: active.config.Policy.SessionID, PolicyVersion: active.config.Policy.Version}
	// Denials use 403, never 401: a 401 would make agents start an OAuth login against us.
	deny := func(status int, reason string) {
		event.ReasonCode, event.Status = reason, status
		event.DurationMS = g.now().Sub(started).Milliseconds()
		g.deny(event)
		writeProblem(w, status, reason)
	}
	id, ok := strings.CutPrefix(r.URL.Path, "/mcp/")
	connector, found := active.connectors[id]
	if !ok || !found || r.URL.RawQuery != "" {
		deny(http.StatusNotFound, "mcp_unknown_service")
		return
	}
	event.ServiceID = id
	if connector.Remote {
		event.Host = remoteHost(connector.Upstream)
	}
	values := r.Header.Values("Authorization")
	if len(values) == 0 {
		deny(http.StatusForbidden, "credential_missing")
		return
	}
	if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte("Bearer "+connector.Placeholder)) != 1 {
		deny(http.StatusForbidden, "credential_mismatch")
		return
	}
	var body []byte
	var msg mcpMessage
	switch r.Method {
	case http.MethodPost:
		data, err := io.ReadAll(io.LimitReader(r.Body, maxMCPBody+1))
		if err != nil || len(data) > maxMCPBody {
			deny(http.StatusRequestEntityTooLarge, "body_too_large")
			return
		}
		if msg, err = parseMCPMessage(data); err != nil {
			deny(http.StatusBadRequest, "mcp_invalid_message")
			return
		}
		body = data
		// Agent-chosen strings are only recorded when they look like a method or tool name.
		event.Method = ""
		if mcpMethodPattern.MatchString(msg.Method) {
			event.Method = msg.Method
		}
		if msg.Method == "tools/call" && toolPattern.MatchString(msg.Tool) {
			event.ToolID = msg.Tool
		}
		if mcpProbes[msg.Method] && len(msg.ID) > 0 {
			writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID,
				"error": map[string]any{"code": -32601, "message": "Method not found"}})
			return
		}
		if field := remoteEmptyLists[msg.Method]; connector.Remote && field != "" && len(msg.ID) > 0 {
			writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{field: []any{}}})
			return
		}
		allowed := mcpMethods
		if connector.Remote {
			allowed = remoteMethods
		}
		if msg.Method != "" && !strings.HasPrefix(msg.Method, "notifications/") && !allowed[msg.Method] {
			g.denyRPC(w, msg, event, started, "mcp_method_denied", "AIofCage 门卫不允许这个 MCP 方法")
			return
		}
		if msg.Method == "tools/call" && !connector.allows(msg.Tool) {
			g.denyRPC(w, msg, event, started, "mcp_tool_denied", toolDeniedText(connector))
			return
		}
	case http.MethodDelete:
		event.Method = "session/close"
	default:
		w.Header().Set("Allow", "POST, DELETE")
		writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	g.forwardMCP(w, r, connector, msg, body, event, started)
}

// toolDeniedText tells the agent what it may use, so it can explain the refusal to the user.
func toolDeniedText(connector Connector) string {
	const hint = "；需要其他工具时，请在 AIofCage 的沙箱 MCP 设置里勾选"
	if len(connector.Tools) == 0 {
		return "AIofCage 门卫拒绝了这个工具调用：该服务目前没有开放任何工具" + hint
	}
	names, more := connector.Tools, ""
	if len(names) > 12 {
		names, more = names[:12], fmt.Sprintf(" 等 %d 个", len(connector.Tools))
	}
	return "AIofCage 门卫拒绝了这个工具调用：该服务只开放 " + strings.Join(names, "、") + more + hint
}

// denyRPC answers a refused message with a JSON-RPC error so the agent can tell the user why.
func (g *Gate) denyRPC(w http.ResponseWriter, msg mcpMessage, event audit.Event, started time.Time, reason, text string) {
	event.Decision, event.ReasonCode, event.Status = audit.DecisionDeny, reason, http.StatusForbidden
	event.DurationMS = g.now().Sub(started).Milliseconds()
	g.deny(event)
	writeRPCError(w, msg, http.StatusForbidden, reason, text)
}

// failRPC answers a message the service could not handle safely, e.g. a rejected credential.
func (g *Gate) failRPC(w http.ResponseWriter, msg mcpMessage, event audit.Event, started time.Time, reason, text string) {
	event.Decision, event.ReasonCode, event.Status = audit.DecisionError, reason, http.StatusBadGateway
	g.recordMCP(event, started, true)
	writeRPCError(w, msg, http.StatusBadGateway, reason, text)
}

// writeRPCError answers a request with a JSON-RPC error; notifications and session closes have
// no ID to answer, so they get a plain problem response.
func writeRPCError(w http.ResponseWriter, msg mcpMessage, status int, reason, text string) {
	if len(msg.ID) == 0 || string(msg.ID) == "null" {
		writeProblem(w, status, reason)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID,
		"error": map[string]any{"code": -32001, "message": text}})
}

func remoteHost(upstream string) string {
	u, err := url.Parse(upstream)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (g *Gate) forwardMCP(w http.ResponseWriter, r *http.Request, connector Connector, msg mcpMessage, body []byte, event audit.Event, started time.Time) {
	parent := r.Context()
	if r.Method == http.MethodDelete {
		// Clients often send the session close while exiting and hang up at once; the service
		// must still end the session (for a connector, its server process).
		parent = context.WithoutCancel(parent)
	}
	ctx, cancel := context.WithTimeout(parent, mcpCallTimeout)
	defer cancel()
	target, client := strings.TrimSuffix(connector.Upstream, "/")+"/mcp", g.mcpClient
	if connector.Remote {
		// The exact endpoint the user configured, through the proxy's public-only dialer.
		target, client = connector.Upstream, g.remoteMCP
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal_error")
		return
	}
	for _, name := range mcpForwardHeaders {
		if value := r.Header.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	if r.Method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if connector.Token != "" {
		req.Header.Set("Authorization", "Bearer "+connector.Token)
	}
	event.BytesIn = int64(len(body))
	resp, err := client.Do(req)
	if err != nil && r.Context().Err() != nil {
		// The agent hung up before the answer: not a service failure.
		event.Decision, event.ReasonCode = audit.DecisionAllow, "client_closed"
		g.recordMCP(event, started, mcpRecorded[event.Method])
		return
	}
	if err != nil {
		reason := "upstream_failed"
		if errors.Is(err, errDestinationDenied) {
			reason = "destination_denied"
		}
		event.Decision, event.ReasonCode, event.Status = audit.DecisionError, reason, http.StatusBadGateway
		g.recordMCP(event, started, true)
		writeProblem(w, http.StatusBadGateway, reason)
		return
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// The credential is ours, never the agent's: an auth challenge would make the agent start
		// an OAuth login against the gateway. The user renews it in the app instead.
		g.failRPC(w, msg, event, started, "upstream_unauthorized",
			"MCP 服务不接受 AIofCage 保存的凭据，请在 AIofCage 的「工具连接」里重新登录或更新令牌")
		return
	case status >= 300 && status < 400:
		// Redirects are not followed and their targets are not revealed to the agent.
		g.failRPC(w, msg, event, started, "redirect_denied", "MCP 服务返回了跳转，AIofCage 门卫不跟随跳转")
		return
	}
	reply := mcpReply{method: msg.Method, connector: connector}
	var out []byte
	streamed := false
	switch {
	case r.Method == http.MethodDelete:
		// A session close carries no data the agent needs.
	case status >= 400:
		// Error bodies may echo the request; decode and mask the token before returning them.
		var withheld bool
		out, withheld = readErrorBody(resp, connector.Token)
		if withheld {
			resp.Header.Set("Content-Type", "application/json")
		}
	case !reply.rewrites():
		streamed = true
	case strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream"):
		streamed = true
	default:
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxMCPReply+1))
		if err == nil && len(data) > maxMCPReply {
			err = errMCPReply
		}
		if err == nil && len(bytes.TrimSpace(data)) > 0 {
			data, err = reply.body(data)
		}
		if err != nil {
			g.failRPC(w, msg, event, started, "mcp_invalid_reply", "MCP 服务的回复无法检查，AIofCage 门卫没有转交")
			return
		}
		out = data
	}
	for _, name := range []string{"Content-Type", "Mcp-Session-Id"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	if !streamed {
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	}
	w.WriteHeader(status)
	var written int64
	var copyErr error
	switch {
	case !streamed:
		n, err := w.Write(out)
		written, copyErr = int64(n), err
	case reply.rewrites():
		written, copyErr = copyEvents(w, resp.Body, reply.message)
	default:
		written, copyErr = copyFlushing(w, resp.Body)
	}
	event.Status, event.BytesOut = status, written
	event.Decision, event.ReasonCode = audit.DecisionAllow, "mcp_forwarded"
	// Closing a session that already ended (404) is routine, e.g. a second close on exit.
	failed := status >= 400 && !(r.Method == http.MethodDelete && status == http.StatusNotFound)
	if failed {
		event.Decision, event.ReasonCode = audit.DecisionError, "upstream_status"
	} else if errors.Is(copyErr, errMCPReply) {
		// The stream was cut where an event could not be checked.
		event.Decision, event.ReasonCode = audit.DecisionError, "mcp_invalid_reply"
		failed = true
	}
	g.recordMCP(event, started, mcpRecorded[event.Method] || failed)
}

func (g *Gate) recordMCP(event audit.Event, started time.Time, visible bool) {
	if !visible {
		return
	}
	event.DurationMS = g.now().Sub(started).Milliseconds()
	g.mu.Lock()
	g.stats.Requests++
	g.mu.Unlock()
	g.events.append(event)
}

// copyFlushing streams the connector's answer, flushing so SSE responses are not held back.
func copyFlushing(w http.ResponseWriter, body io.Reader) (int64, error) {
	controller := http.NewResponseController(w)
	buffer := make([]byte, 32<<10)
	var total int64
	for {
		n, err := body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return total, writeErr
			}
			total += int64(n)
			_ = controller.Flush()
		}
		if err != nil {
			if err == io.EOF {
				return total, nil
			}
			return total, err
		}
	}
}
