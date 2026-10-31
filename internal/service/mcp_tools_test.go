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

	"github.com/shaoxier/AIofCage/internal/sandbox"
)

func TestRemoteServiceToolListAndNoAuth(t *testing.T) {
	app, secrets, st := newApp(t)
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
	if _, err := app.SaveMCPService(SaveMCPServiceRequest{ID: view.ID, Name: "Docs", URL: "https://example.com/mcp", Token: "short"}); err == nil {
		t.Fatal("a token this short would be masked inside ordinary words of every reply")
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
	if err := st.SaveSandbox(sandbox.Sandbox{ID: "sbx-docs0001", Name: "docs", MCP: []string{view.ID},
		MCPPermissions: map[string][]string{view.ID: {"delete"}}, MCPGrantURLs: map[string]string{view.ID: "https://example.com/mcp"}}); err != nil {
		t.Fatal(err)
	}
	if views, _ := app.ListSandboxes(); fmt.Sprint(views[0].MCPPermissions) != "map["+view.ID+":[delete]]" {
		t.Fatalf("a grant for the current address applies: %v", views[0].MCPPermissions)
	}
	moved, err := app.SaveMCPService(SaveMCPServiceRequest{ID: view.ID, Name: "Docs", URL: "https://example.com/other", NoAuth: true})
	if err != nil || moved.ToolsError == "" || len(moved.Tools) != 0 || moved.CatalogAt != nil {
		t.Fatalf("another address must not keep the old tool list: %+v %v", moved, err)
	}
	if views, _ := app.ListSandboxes(); len(views[0].MCPPermissions) != 0 {
		t.Fatalf("a grant made for the old address must not apply to the new one: %v", views[0].MCPPermissions)
	}
	if _, err := app.RefreshMCPTools(view.ID); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("refresh reports why the list is unavailable: %v", err)
	}
}

// A credential belongs to the host it was given for: moving a service to another domain needs a
// new token or a new login, so a typo in the address never sends the old one there.
func TestCredentialsDoNotFollowAServiceToAnotherHost(t *testing.T) {
	app, secrets, _ := newApp(t)
	view, err := app.SaveMCPService(SaveMCPServiceRequest{Name: "Docs", URL: "https://mcp.example.com/mcp", Preset: "custom", Token: "docs-token-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.SaveMCPService(SaveMCPServiceRequest{ID: view.ID, Name: "Docs", URL: "https://mcp.example.com/v2/mcp"}); err != nil {
		t.Fatalf("another path on the same host keeps the token: %v", err)
	}
	_, err = app.SaveMCPService(SaveMCPServiceRequest{ID: view.ID, Name: "Docs", URL: "https://mcp.exarnple.com/mcp"})
	if err == nil || !strings.Contains(err.Error(), "重新填写访问令牌") {
		t.Fatalf("another host must not get the saved token: %v", err)
	}
	if moved, err := app.SaveMCPService(SaveMCPServiceRequest{ID: view.ID, Name: "Docs", URL: "https://mcp.other.com/mcp", Token: "other-token-2"}); err != nil ||
		string(secrets[moved.KeyRef]) != "other-token-2" {
		t.Fatalf("a new token for the new host is accepted: %v", err)
	}
	notion, err := app.SaveMCPService(SaveMCPServiceRequest{Name: "Notion", Kind: "oauth", URL: "https://mcp.notion.com/mcp", Preset: "notion"})
	if err != nil {
		t.Fatal(err)
	}
	secrets[notion.KeyRef] = []byte(`{"client_id":"c","token_endpoint":"https://mcp.notion.com/token","access_token":"synthetic-access-0001","refresh_token":"synthetic-refresh-0001"}`)
	moved, err := app.SaveMCPService(SaveMCPServiceRequest{ID: notion.ID, Name: "Notion", Kind: "oauth", URL: "https://mcp.notlon.com/mcp"})
	if _, kept := secrets[notion.KeyRef]; err != nil || kept || moved.KeySaved {
		t.Fatalf("a login for another host is removed, so the next step is a new login: %+v %v", moved, err)
	}
}
