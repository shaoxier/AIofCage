package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/oauth"
)

func TestOAuthServiceRefreshesBeforeUse(t *testing.T) {
	app, secrets, _ := newApp(t)
	var mcpAuth string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-1" {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "access-2", "refresh_token": "refresh-2", "expires_in": 3600, "token_type": "Bearer"})
		case "/mcp":
			mcpAuth = r.Header.Get("Authorization")
			var msg struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			json.NewDecoder(r.Body).Decode(&msg)
			switch msg.Method {
			case "initialize":
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"fake"}}}`, msg.ID)
			case "tools/list":
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"search","annotations":{"readOnlyHint":true}},{"name":"create_page"}]}}`, msg.ID)
			default:
				w.WriteHeader(http.StatusAccepted)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	// httptest's certificate covers example.com; route that name to the test server.
	transport := server.Client().Transport.(*http.Transport).Clone()
	address := server.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (netConn, error) {
		return (&dialer{}).DialContext(ctx, network, address)
	}
	app.client = &http.Client{Transport: transport}
	app.oauth = oauth.NewClient(app.client, func(string) error { return nil })

	view, err := app.SaveMCPService(SaveMCPServiceRequest{Name: "Notion", Kind: "oauth", URL: "https://example.com/mcp",
		Preset: "notion", Token: "pasted-token-is-ignored"})
	if err != nil || view.KeySaved || view.ID != "notion" {
		t.Fatalf("an OAuth service has no credentials until login: %+v %v", view, err)
	}
	if _, ok := secrets[view.KeyRef]; ok {
		t.Fatal("a pasted token must not be stored for OAuth services")
	}
	expired, _ := json.Marshal(oauth.Credentials{ClientID: "c", TokenEndpoint: "https://example.com/token",
		AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: time.Now().Add(-time.Minute)})
	secrets[view.KeyRef] = expired
	views, _ := app.ListMCPServices()
	if len(views) != 1 || !views[0].KeySaved {
		t.Fatalf("a refresh token keeps the login usable: %+v", views)
	}
	result, err := app.TestMCPService(view.ID)
	if err != nil || !result.OK || mcpAuth != "Bearer access-2" || !strings.Contains(result.Message, "2 个工具，其中 1 个") {
		t.Fatalf("test must refresh first: %+v %v %q", result, err, mcpAuth)
	}
	if listed, _ := app.ListMCPServices(); len(listed[0].Tools) != 2 || len(listed[0].DefaultTools) != 1 || listed[0].DefaultTools[0] != "search" {
		t.Fatalf("testing a remote service updates its tool list: %+v", listed[0])
	}
	saved, _ := oauth.Parse(secrets[view.KeyRef])
	if saved.AccessToken != "access-2" || saved.RefreshToken != "refresh-2" {
		t.Fatalf("refreshed credentials must be saved: %+v", saved)
	}
	saved.RefreshToken, saved.ExpiresAt = "", time.Now().Add(-time.Minute)
	secrets[view.KeyRef], _ = json.Marshal(saved)
	if views, _ = app.ListMCPServices(); views[0].KeySaved {
		t.Fatal("an expired login without refresh token needs a new login")
	}
	if result, _ = app.TestMCPService(view.ID); result.OK || !strings.Contains(result.Message, "重新登录") {
		t.Fatalf("expired login: %+v", result)
	}
	github, _ := app.SaveMCPService(SaveMCPServiceRequest{Name: "GitHub", URL: "https://api.githubcopilot.com/mcp/", Preset: "github", Token: "synthetic-token-1"})
	if _, err := app.AuthorizeMCPService(github.ID); err == nil {
		t.Fatal("token services have no OAuth login")
	}
}

func TestDisconnectOAuthServiceRemovesLocalCredentials(t *testing.T) {
	app, secrets, _ := newApp(t)
	view, err := app.SaveMCPService(SaveMCPServiceRequest{Name: "Notion", Kind: "oauth", URL: "https://example.com/mcp", Preset: "notion"})
	if err != nil {
		t.Fatal(err)
	}
	secrets[view.KeyRef] = []byte(`{"access_token":"access-1","refresh_token":"refresh-1"}`)
	if _, err := app.DisconnectMCPService(view.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := secrets[view.KeyRef]; ok {
		t.Fatal("OAuth credentials remain after disconnect")
	}
	updated, err := app.ListMCPServices()
	if err != nil || len(updated) != 1 || updated[0].KeySaved {
		t.Fatalf("disconnected service still appears authorized: %+v %v", updated, err)
	}
	github, err := app.SaveMCPService(SaveMCPServiceRequest{Name: "GitHub", URL: "https://api.githubcopilot.com/mcp/", Preset: "github", Token: "synthetic-token-3"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.DisconnectMCPService(github.ID); err == nil || !strings.Contains(err.Error(), "OAuth") {
		t.Fatalf("token service accepted OAuth disconnect: %v", err)
	}
}

// A browser login takes minutes; its credential is kept only if the service still has the address
// it was obtained for.
func TestLoginForAnEditedServiceIsNotSaved(t *testing.T) {
	app, secrets, st := newApp(t)
	view, err := app.SaveMCPService(SaveMCPServiceRequest{Name: "Notion", Kind: "oauth", URL: "https://mcp.notion.com/mcp", Preset: "notion"})
	if err != nil {
		t.Fatal(err)
	}
	startedFor, _ := st.MCPService(view.ID)
	if _, err := app.SaveMCPService(SaveMCPServiceRequest{ID: view.ID, Name: "Notion", Kind: "oauth", URL: "https://mcp.notion.example/mcp"}); err != nil {
		t.Fatal(err)
	}
	if err := app.saveLogin(context.Background(), startedFor, []byte(`{"access_token":"synthetic-access-0002"}`)); err == nil {
		t.Fatal("a login for the old address must not be saved for the new one")
	}
	if _, saved := secrets[view.KeyRef]; saved {
		t.Fatal("nothing may be stored")
	}
}
