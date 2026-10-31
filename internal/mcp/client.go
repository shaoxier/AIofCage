package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The host lists a remote service's tools itself, with the service's token, so the user can
// choose what to grant before any sandbox uses the service. Agents never take this path.

const (
	clientProtocol = "2025-06-18"
	maxToolPages   = 20
	maxAnswerBytes = 8 << 20
)

var (
	// ErrInvalidAnswer means the service answered with something that is not MCP.
	ErrInvalidAnswer = errors.New("MCP 服务的回复格式无效")
	versionPattern   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	sessionPattern   = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)
)

// StatusError is an HTTP status that is not a usable MCP answer.
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("MCP 服务返回了 HTTP %d", e.Status) }

// RPCError is an error answer from the service.
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string { return "MCP 服务返回错误：" + e.Message }

// ListTools asks a remote MCP service for its tools the way an agent would: initialize, the
// initialized notification, tools/list page by page, then the session close. Redirects are never
// followed, so the token only ever goes to the configured endpoint.
func ListTools(ctx context.Context, client *http.Client, endpoint, token string) ([]Tool, error) {
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s := &clientSession{client: &noRedirect, endpoint: endpoint, token: token}
	defer s.close()
	result, err := s.call(ctx, "initialize", map[string]any{"protocolVersion": clientProtocol, "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "AIofCage", "version": "1"}})
	if err != nil {
		return nil, err
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(result, &initialized) != nil {
		return nil, ErrInvalidAnswer
	}
	s.version = clientProtocol
	if versionPattern.MatchString(initialized.ProtocolVersion) {
		s.version = initialized.ProtocolVersion
	}
	if err := s.notify(ctx, "notifications/initialized"); err != nil {
		return nil, err
	}
	tools := []Tool{}
	seen := map[string]bool{}
	cursor := ""
	for range maxToolPages {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := s.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if json.Unmarshal(result, &page) != nil {
			return nil, ErrInvalidAnswer
		}
		for _, raw := range page.Tools {
			tool, ok := parseTool(raw)
			if ok && !seen[tool.Name] && len(tools) < MaxCatalogTools {
				seen[tool.Name] = true
				tools = append(tools, tool)
			}
		}
		if page.NextCursor == "" || len(tools) >= MaxCatalogTools {
			break
		}
		cursor = page.NextCursor
	}
	return tools, nil
}

// parseTool reads the name by its exact key, as the agent's client and the gateway do.
func parseTool(raw json.RawMessage) (Tool, bool) {
	var fields map[string]json.RawMessage
	var tool Tool
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(fields["name"], &tool.Name) != nil || !ToolNamePattern.MatchString(tool.Name) {
		return tool, false
	}
	var details struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Annotations struct {
			Title           string `json:"title"`
			ReadOnlyHint    *bool  `json:"readOnlyHint"`
			DestructiveHint *bool  `json:"destructiveHint"`
		} `json:"annotations"`
	}
	_ = json.Unmarshal(raw, &details)
	tool.Title = clip(firstNonEmpty(details.Title, details.Annotations.Title), 80)
	tool.Description = clip(details.Description, 240)
	tool.ReadOnly = details.Annotations.ReadOnlyHint != nil && *details.Annotations.ReadOnlyHint
	tool.Destructive = !tool.ReadOnly && details.Annotations.DestructiveHint != nil && *details.Annotations.DestructiveHint
	return tool, true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func clip(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if runes := []rune(value); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return value
}

type clientSession struct {
	client              *http.Client
	endpoint, token, id string
	version             string
	next                int
}

func (s *clientSession) send(ctx context.Context, method string, payload any) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.endpoint, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	if s.version != "" {
		req.Header.Set("Mcp-Protocol-Version", s.version)
	}
	if s.id != "" {
		req.Header.Set("Mcp-Session-Id", s.id)
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	return s.client.Do(req)
}

func (s *clientSession) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.next++
	id := s.next
	resp, err := s.send(ctx, http.MethodPost, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Status: resp.StatusCode}
	}
	if session := resp.Header.Get("Mcp-Session-Id"); s.id == "" && sessionPattern.MatchString(session) {
		s.id = session
	}
	body := io.LimitReader(resp.Body, maxAnswerBytes)
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		data, err := io.ReadAll(body)
		if err != nil {
			return nil, err
		}
		result, found, err := answerFor(data, id)
		if err == nil && !found {
			err = ErrInvalidAnswer
		}
		return result, err
	}
	// An event stream may carry notifications before the answer.
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), maxAnswerBytes)
	var data []string
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(value, " "))
			continue
		}
		if line != "" || len(data) == 0 {
			continue
		}
		result, found, err := answerFor([]byte(strings.Join(data, "\n")), id)
		if err != nil || found {
			return result, err
		}
		data = nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, ErrInvalidAnswer
}

// answerFor returns the result of the answer to request id; other messages are not found.
func answerFor(data []byte, id int) (json.RawMessage, bool, error) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &msg) != nil {
		return nil, false, ErrInvalidAnswer
	}
	if string(msg.ID) != strconv.Itoa(id) {
		return nil, false, nil
	}
	if msg.Error != nil {
		return nil, true, &RPCError{Code: msg.Error.Code, Message: clip(msg.Error.Message, 200)}
	}
	if len(msg.Result) == 0 {
		return nil, true, ErrInvalidAnswer
	}
	return msg.Result, true, nil
}

func (s *clientSession) notify(ctx context.Context, method string) error {
	resp, err := s.send(ctx, http.MethodPost, map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		return &StatusError{Status: resp.StatusCode}
	}
	return nil
}

// close ends the session on the service; a failure only leaves it to expire.
func (s *clientSession) close() {
	if s.id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if resp, err := s.send(ctx, http.MethodDelete, nil); err == nil {
		resp.Body.Close()
	}
}
