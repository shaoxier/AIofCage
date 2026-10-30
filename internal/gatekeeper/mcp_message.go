package gatekeeper

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Messages are read the way the receiving server reads them. Go matches object keys to struct
// fields case-insensitively, with Unicode folding ("paramſ" is "params"), while JavaScript and
// Python match exactly; all of them keep the last of repeated keys. A message whose envelope or
// tools/call parameters repeat a key, or spell a known key in another case, is refused: otherwise
// the gatekeeper could check one method or tool name while the server runs another.

var (
	errMCPMessage   = errors.New("mcp_invalid_message")
	errMCPReply     = errors.New("mcp_invalid_reply")
	mcpEnvelopeKeys = []string{"jsonrpc", "id", "method", "params", "result", "error"}
	mcpCallKeys     = []string{"name", "arguments", "_meta"}
)

const (
	maxMCPKeys  = 64
	maxMCPReply = 16 << 20
)

type mcpMessage struct {
	ID     json.RawMessage
	Method string
	// Tool is the exact params.name of a tools/call.
	Tool string
}

func parseMCPMessage(data []byte) (mcpMessage, error) {
	var msg mcpMessage
	if !utf8.Valid(data) {
		return msg, errMCPMessage
	}
	fields, err := objectFields(data, mcpEnvelopeKeys)
	if err != nil {
		return msg, errMCPMessage
	}
	msg.ID = fields["id"]
	raw, ok := fields["method"]
	if !ok {
		// The client answering a server request (sampling, elicitation, roots).
		_, result := fields["result"]
		_, failure := fields["error"]
		if len(msg.ID) == 0 || result == failure {
			return msg, errMCPMessage
		}
		return msg, nil
	}
	if json.Unmarshal(raw, &msg.Method) != nil || msg.Method == "" {
		return msg, errMCPMessage
	}
	if msg.Method == "tools/call" {
		params, err := objectFields(fields["params"], mcpCallKeys)
		if err != nil || json.Unmarshal(params["name"], &msg.Tool) != nil || msg.Tool == "" {
			return msg, errMCPMessage
		}
	}
	return msg, nil
}

// objectFields returns the members of one JSON object, which must be the whole of data.
func objectFields(data []byte, known []string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errMCPMessage
	}
	fields := map[string]json.RawMessage{}
	folded := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || len(fields) >= maxMCPKeys {
			return nil, errMCPMessage
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, errMCPMessage
		}
		fold := foldKey(key)
		if folded[fold] {
			return nil, errMCPMessage
		}
		folded[fold] = true
		for _, name := range known {
			if key != name && fold == foldKey(name) {
				return nil, errMCPMessage
			}
		}
		fields[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errMCPMessage
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errMCPMessage // a second value after the object
	}
	return fields, nil
}

// foldKey maps a key to one member of its simple case-folding class, the equivalence Go's
// encoding/json uses for field names.
func foldKey(key string) string {
	return strings.Map(func(r rune) rune {
		lowest := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			lowest = min(lowest, f)
		}
		return lowest
	}, key)
}

func (c Connector) allows(tool string) bool {
	if !c.ToolsRestricted && len(c.Tools) == 0 {
		return true // a connector delivered before tool grants existed
	}
	return slices.Contains(c.Tools, tool)
}

// mcpReply rewrites what a service sends back before the agent sees it: a tool list keeps only
// granted tools, a remote service's initialize answer drops the capabilities the gateway does not
// offer, and a remote service's token is masked wherever it appears.
type mcpReply struct {
	method    string
	connector Connector
}

func (r mcpReply) rewrites() bool {
	return r.method == "tools/list" || r.connector.Remote
}

// mask hides a remote service's token in anything relayed from it.
func (r mcpReply) mask(data []byte) []byte {
	if !r.connector.Remote {
		return data
	}
	return redactSecret(data, r.connector.Token)
}

// maskDecoded hides the token where a message writes it with JSON escapes (e.g. "\u0067hp_" or
// "\/"): a byte comparison misses those, but the agent's JSON parser decodes them.
func (r mcpReply) maskDecoded(data []byte) ([]byte, error) {
	secret := r.connector.Token
	if !r.connector.Remote || secret == "" {
		return data, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return data, nil // not JSON: nothing a JSON parser would decode
	}
	if !holdsSecret(value, secret) {
		return data, nil
	}
	return marshalCompact(maskValue(value, secret))
}

func holdsSecret(value any, secret string) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(v, secret)
	case []any:
		return slices.ContainsFunc(v, func(item any) bool { return holdsSecret(item, secret) })
	case map[string]any:
		for key, item := range v {
			if strings.Contains(key, secret) || holdsSecret(item, secret) {
				return true
			}
		}
	}
	return false
}

func maskValue(value any, secret string) any {
	switch v := value.(type) {
	case string:
		return string(redactSecret([]byte(v), secret))
	case []any:
		for i, item := range v {
			v[i] = maskValue(item, secret)
		}
	case map[string]any:
		masked := make(map[string]any, len(v))
		for key, item := range v {
			masked[string(redactSecret([]byte(key), secret))] = maskValue(item, secret)
		}
		return masked
	}
	return value
}

// body rewrites a JSON response: one message, or a batch of them.
func (r mcpReply) body(data []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return r.message(trimmed)
	}
	var batch []json.RawMessage
	if err := json.Unmarshal(trimmed, &batch); err != nil {
		return nil, errMCPReply
	}
	for i, item := range batch {
		out, err := r.message(item)
		if err != nil {
			return nil, err
		}
		batch[i] = out
	}
	return marshalCompact(batch)
}

func (r mcpReply) message(data []byte) ([]byte, error) {
	out := data
	switch {
	case r.method == "tools/list":
		rewritten, err := rewriteResult(data, r.filterTools)
		if err != nil {
			return nil, err
		}
		out = rewritten
	case r.method == "initialize" && r.connector.Remote:
		rewritten, err := rewriteResult(data, stripCapabilities)
		if err != nil {
			return nil, err
		}
		out = rewritten
	}
	return r.maskDecoded(r.mask(out))
}

// rewriteResult applies change to a response's result; errors and notifications pass unchanged.
func rewriteResult(data []byte, change func(map[string]json.RawMessage) error) ([]byte, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, errMCPReply
	}
	raw, ok := msg["result"]
	if !ok {
		return data, nil
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil || result == nil {
		return nil, errMCPReply
	}
	if err := change(result); err != nil {
		return nil, err
	}
	encoded, err := marshalCompact(result)
	if err != nil {
		return nil, err
	}
	msg["result"] = encoded
	return marshalCompact(msg)
}

func (r mcpReply) filterTools(result map[string]json.RawMessage) error {
	var tools []json.RawMessage
	if raw, ok := result["tools"]; ok {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return errMCPReply
		}
	}
	kept := []json.RawMessage{}
	for _, tool := range tools {
		var fields map[string]json.RawMessage
		var name string
		if json.Unmarshal(tool, &fields) == nil && json.Unmarshal(fields["name"], &name) == nil && r.connector.allows(name) {
			kept = append(kept, tool)
		}
	}
	encoded, err := marshalCompact(kept)
	result["tools"] = encoded
	return err
}

// stripCapabilities keeps clients from asking a remote service for resources, prompts or
// completions, which tool grants do not cover; the gateway answers those listings as empty.
func stripCapabilities(result map[string]json.RawMessage) error {
	raw, ok := result["capabilities"]
	if !ok {
		return nil
	}
	var capabilities map[string]json.RawMessage
	if err := json.Unmarshal(raw, &capabilities); err != nil {
		return errMCPReply
	}
	for _, name := range []string{"resources", "prompts", "completions"} {
		delete(capabilities, name)
	}
	encoded, err := marshalCompact(capabilities)
	result["capabilities"] = encoded
	return err
}

func marshalCompact(value any) (json.RawMessage, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, errMCPReply
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// copyEvents relays a server-sent event stream one complete event at a time, passing each
// event's data through rewrite and everything sent through mask. Comments and the other fields
// are kept, and every event is flushed at once so streamed answers are not held back. Lines are
// split as clients split them, and an event cut off by the end of the stream is dropped, as a
// client would drop it.
func copyEvents(w http.ResponseWriter, body io.Reader, rewrite func([]byte) ([]byte, error), mask func([]byte) []byte) (int64, error) {
	controller := http.NewResponseController(w)
	lines := &eventLines{reader: bufio.NewReaderSize(body, 64<<10)}
	var total int64
	var event, data bytes.Buffer
	hasData := false
	send := func(chunk []byte) error {
		if mask != nil {
			chunk = mask(chunk)
		}
		n, err := w.Write(chunk)
		total += int64(n)
		_ = controller.Flush()
		return err
	}
	for {
		line, err := lines.next(maxMCPReply - event.Len() - data.Len())
		if err == io.EOF {
			return total, nil // anything read here lacks its terminator, so its event is incomplete
		}
		if err != nil {
			return total, err
		}
		switch {
		case len(line) == 0:
			if hasData {
				out, err := rewrite(data.Bytes())
				if err != nil {
					return total, err
				}
				// Data spanning several lines keeps one data field per line.
				for _, part := range bytes.Split(out, []byte("\n")) {
					event.WriteString("data: ")
					event.Write(bytes.TrimSuffix(part, []byte("\r")))
					event.WriteByte('\n')
				}
			}
			event.WriteByte('\n')
			if err := send(event.Bytes()); err != nil {
				return total, err
			}
			event.Reset()
			data.Reset()
			hasData = false
		case line[0] == ':' && event.Len() == 0 && !hasData:
			// A keep-alive comment between events.
			if err := send(append(line, '\n')); err != nil {
				return total, err
			}
		default:
			name, value, _ := bytes.Cut(line, []byte(":"))
			if string(name) != "data" {
				event.Write(line)
				event.WriteByte('\n')
				break
			}
			if hasData {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(value, []byte(" ")))
			hasData = true
		}
	}
}

// eventLines splits an event stream into lines the way clients do: a line ends at CRLF, LF or a
// lone CR, and a byte order mark opening the stream is dropped.
type eventLines struct {
	reader  *bufio.Reader
	started bool
	afterCR bool
}

// next returns one line without its terminator; a line longer than limit is an error.
func (l *eventLines) next(limit int) ([]byte, error) {
	if !l.started {
		l.started = true
		if mark, err := l.reader.Peek(3); err == nil && string(mark) == "\xef\xbb\xbf" {
			_, _ = l.reader.Discard(3)
		}
	}
	var line []byte
	for {
		b, err := l.reader.ReadByte()
		if err != nil {
			return line, err
		}
		if l.afterCR {
			l.afterCR = false
			if b == '\n' {
				continue // the LF of a CRLF whose CR already ended the line
			}
		}
		switch b {
		case '\n':
			return line, nil
		case '\r':
			l.afterCR = true
			return line, nil
		}
		if len(line) >= limit {
			return nil, errMCPReply
		}
		line = append(line, b)
	}
}
