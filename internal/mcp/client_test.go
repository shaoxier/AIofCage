package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestListToolsFollowsTheProtocol(t *testing.T) {
	var steps []string
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer service-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			deleted.Store(r.Header.Get("Mcp-Session-Id") == "s-1")
			return
		}
		var msg struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &msg)
		steps = append(steps, msg.Method+"|"+r.Header.Get("Mcp-Session-Id")+"|"+r.Header.Get("Mcp-Protocol-Version"))
		switch msg.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s-1")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`, msg.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "text/event-stream")
			if !strings.Contains(string(msg.Params), "page-2") {
				fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\"}\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\n"+
					"data: \"result\":{\"tools\":[{\"name\":\"get_issue\",\"title\":\"Get issue\",\"annotations\":{\"readOnlyHint\":true}},"+
					"{\"name\":\"bad name\"},{\"Name\":\"wrong_case\"}],\"nextCursor\":\"page-2\"}}\n\n", msg.ID)
				return
			}
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"tools\":[{\"name\":\"delete_repo\",\"description\":\"Deletes   a\\nrepository\","+
				"\"annotations\":{\"destructiveHint\":true}},{\"name\":\"get_issue\"}]}}\n\n", msg.ID)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tools, err := ListTools(ctx, server.Client(), server.URL+"/mcp", "service-token")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0] != (Tool{Name: "get_issue", Title: "Get issue", ReadOnly: true}) ||
		tools[1] != (Tool{Name: "delete_repo", Description: "Deletes a repository", Destructive: true}) {
		t.Fatalf("tools %+v", tools)
	}
	want := "initialize||,notifications/initialized|s-1|2025-03-26,tools/list|s-1|2025-03-26,tools/list|s-1|2025-03-26"
	if strings.Join(steps, ",") != want {
		t.Fatalf("steps %v", steps)
	}
	if !deleted.Load() {
		t.Fatal("the session must be closed")
	}
}

func TestListToolsErrors(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Store(true) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/denied":
			http.Error(w, "no", http.StatusUnauthorized)
		case "/moved":
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		case "/failing":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"bad   request"}}`)
		case "/html":
			fmt.Fprint(w, `<html>login</html>`)
		}
	}))
	defer server.Close()
	ctx := context.Background()
	var status *StatusError
	if _, err := ListTools(ctx, server.Client(), server.URL+"/denied", "t"); !errors.As(err, &status) || status.Status != 401 {
		t.Fatalf("401: %v", err)
	}
	if _, err := ListTools(ctx, server.Client(), server.URL+"/moved", "t"); !errors.As(err, &status) || status.Status != 307 || redirected.Load() {
		t.Fatalf("redirects must not be followed with the token: %v", err)
	}
	var rpc *RPCError
	if _, err := ListTools(ctx, server.Client(), server.URL+"/failing", "t"); !errors.As(err, &rpc) || rpc.Message != "bad request" {
		t.Fatalf("rpc error: %v", err)
	}
	if _, err := ListTools(ctx, server.Client(), server.URL+"/html", ""); !errors.Is(err, ErrInvalidAnswer) {
		t.Fatalf("html: %v", err)
	}
}
