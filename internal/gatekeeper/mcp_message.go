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
	if r.connector.Remote {
		out = redactSecret(out, r.connector.Token)
	}
	return out, nil
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
// event's data through rewrite. Comments and the other fields are kept, and every event is
// flushed at once so streamed answers are not held back. An event cut off by the end of the
// stream is dropped, as a client would drop it.
func copyEvents(w http.ResponseWriter, body io.Reader, rewrite func([]byte) ([]byte, error)) (int64, error) {
	controller := http.NewResponseController(w)
	reader := bufio.NewReaderSize(body, 64<<10)
	var total int64
	var event, data bytes.Buffer
	hasData := false
	send := func(chunk []byte) error {
		n, err := w.Write(chunk)
		total += int64(n)
		_ = controller.Flush()
		return err
	}
	for {
		line, err := readEventLine(reader)
		if err == io.EOF {
			return total, nil // anything read here lacks its terminator, so its event is incomplete
		}
		if err != nil {
			return total, err
		}
		text := bytes.TrimRight(line, "\r\n")
		switch {
		case len(text) == 0:
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
		case text[0] == ':' && event.Len() == 0 && !hasData:
			// A keep-alive comment between events.
			if err := send(append(text, '\n')); err != nil {
				return total, err
			}
		default:
			name, value, _ := bytes.Cut(text, []byte(":"))
			if string(name) != "data" {
				event.Write(text)
				event.WriteByte('\n')
				break
			}
			if hasData {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(value, []byte(" ")))
			hasData = true
			if data.Len()+event.Len() > maxMCPReply {
				return total, errMCPReply
			}
		}
	}
}

// readEventLine reads one line, terminator included, of at most maxMCPReply bytes.
func readEventLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(line)+len(chunk) > maxMCPReply {
			return nil, errMCPReply
		}
		line = append(line, chunk...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}
