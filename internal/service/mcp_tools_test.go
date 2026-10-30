package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRemoteServiceToolListAndNoAuth(t *testing.T) {
	app, secrets, _ := newApp(t)
	var mu sync.Mutex
	var auth []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		var msg struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&msg)
		switch msg.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18"}}`, msg.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"search","annotations":{"readOnlyHint":true}},{"name":"delete"}]}}`, msg.ID)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	address := server.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (netConn, error) {
		return (&dialer{}).DialContext(ctx, network, address)
	}
	client := *app.client
	client.Transport = transport
	app.client = &client
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		values := auth
		auth = nil
		return values
	}

	view, err := app.SaveMCPService(SaveMCPServiceRequest{Name: "Docs", URL: "https://example.com/mcp", Preset: "custom", NoAuth: true})
	if err != nil || view.Auth != "none" || !view.KeySaved || view.ToolsError != "" || len(view.Tools) != 2 ||
		strings.Join(view.DefaultTools, ",") != "search" {
		t.Fatalf("a public service is saved with its tools listed: %+v %v", view, err)
	}
	for _, value := range seen() {
		if value != "" {
			t.Fatalf("a public service must not receive a token: %q", value)
		}
	}
	if _, err := app.SaveMCPService(SaveMCPServiceRequest{ID: view.ID, Name: "Docs", URL: "https://example.com/mcp"}); err == nil {
		t.Fatal("switching to a token without giving one must be refused")
	}
	if _, err := app.SaveMCPService(SaveMCPServiceRequest{ID: view.ID, Name: "Docs", URL: "https://example.com/mcp", Token: "docs-token-1"}); err != nil {
		t.Fatal(err)
	}
	if values := seen(); len(values) == 0 || values[0] != "Bearer docs-token-1" {
		t.Fatalf("the tool list is fetched with the saved token: %q", values)
	}
	if _, err := app.SaveMCPService(SaveMCPServiceRequest{ID: view.ID, Name: "Docs", URL: "https://example.com/mcp", NoAuth: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := secrets[view.KeyRef]; ok {
		t.Fatal("a token is removed when the service no longer needs one")
	}
	moved, err := app.SaveMCPService(SaveMCPServiceRequest{ID: view.ID, Name: "Docs", URL: "https://example.com/other", NoAuth: true})
	if err != nil || moved.ToolsError == "" || len(moved.Tools) != 0 || moved.CatalogAt != nil {
		t.Fatalf("another address must not keep the old tool list: %+v %v", moved, err)
	}
	if _, err := app.RefreshMCPTools(view.ID); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("refresh reports why the list is unavailable: %v", err)
	}
}
