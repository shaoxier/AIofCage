package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/gatekeeper"
	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
	"github.com/shaoxier/AIofCage/internal/store"
)

type memoryVault map[string][]byte

func (v memoryVault) Put(_ context.Context, ref string, value []byte) error {
	v[ref] = append([]byte(nil), value...)
	return nil
}

func (v memoryVault) Read(_ context.Context, ref string) ([]byte, error) {
	if value, ok := v[ref]; ok {
		return append([]byte(nil), value...), nil
	}
	return nil, errors.New("not found")
}

func (v memoryVault) Delete(_ context.Context, ref string) error {
	delete(v, ref)
	return nil
}

// mcpServer serves https://docs.example.com/mcp; tools can change between listings.
func mcpServer(t *testing.T, tools *atomic.Value) *http.Client {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &msg)
		w.Header().Set("Content-Type", "application/json")
		switch msg.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}}}}`, msg.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":%s}}`, msg.ID, tools.Load().(string))
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(server.Close)
	client := server.Client()
	client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	return client
}

func newRemoteManager(t *testing.T, client *http.Client) (*Manager, *store.Store, memoryVault) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secrets := memoryVault{}
	return &Manager{store: st, vault: secrets, http: client, gates: map[string]*gateClient{}, locks: map[string]*sync.Mutex{},
		connectorTokens: map[string]string{}}, st, secrets
}

func TestRemoteMCPReachesOnlyTheGateway(t *testing.T) {
	m, st, secrets := newRemoteManager(t, nil)
	github := mcp.Service{ID: "github", Name: "GitHub", URL: "https://api.githubcopilot.com/mcp/", KeyRef: mcp.KeyRefFor("github"),
		Catalog: []mcp.Tool{{Name: "get_issue", ReadOnly: true}, {Name: "create_issue"}}}
	if err := st.SaveMCPService(github); err != nil {
		t.Fatal(err)
	}
	secrets[github.KeyRef] = []byte("synthetic-github-pat-0123456789")
	sbx := sandbox.Sandbox{ID: "sbx-remote01", Name: "r", ProjectPath: "/Users/u/CageWorkspace/p", MCP: []string{"github"},
		Bindings: []sandbox.Binding{{Agent: session.Codex}}, Placeholders: map[string]string{"mcp:github": "cage-GGGGGGGGGGGGGGGGGGGG"},
		MCPPermissions: map[string][]string{"github": {"get_issue"}}}
	config, err := m.gateConfig(context.Background(), sbx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Secrets) != 0 || len(config.Policy.Rules) != 0 {
		t.Fatalf("the proxy must get neither the token nor a rule for the service: %+v", config)
	}
	want := gatekeeper.Connector{ID: "github", Placeholder: "cage-GGGGGGGGGGGGGGGGGGGG", Upstream: "https://api.githubcopilot.com/mcp/",
		Token: "synthetic-github-pat-0123456789", Tools: []string{"get_issue"}, ToolsRestricted: true, Remote: true}
	if len(config.Connectors) != 1 || fmt.Sprint(config.Connectors[0]) != fmt.Sprint(want) {
		t.Fatalf("route %+v", config.Connectors)
	}
	// The real gatekeeper accepts what the app delivers.
	token := "c3ludGhldGljLW1hbmFnZW1lbnQtdG9rZW4tMDEyMzQ1Njc4OWFiY2RlZg"
	handlers, err := gatekeeper.NewHandlers(token)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(config)
	r := httptest.NewRequest(http.MethodPut, "/config", strings.NewReader(string(data)))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handlers.Management.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("gatekeeper refused the config: %d %s", w.Code, w.Body.String())
	}
}

func TestRemoteToolsArePinnedSoNewToolsStayClosed(t *testing.T) {
	var tools atomic.Value
	tools.Store(`[{"name":"search_docs","annotations":{"readOnlyHint":true}},{"name":"delete_page"}]`)
	m, st, _ := newRemoteManager(t, mcpServer(t, &tools))
	docs := mcp.Service{ID: "docs", Name: "Docs", URL: "https://docs.example.com/mcp", KeyRef: mcp.KeyRefFor("docs"), Auth: mcp.AuthNone}
	if err := st.SaveMCPService(docs); err != nil {
		t.Fatal(err)
	}
	sbx := sandbox.Sandbox{ID: "sbx-remote02", Name: "r", ProjectPath: "/Users/u/CageWorkspace/p", MCP: []string{"docs"},
		Bindings: []sandbox.Binding{{Agent: session.Codex}}, Placeholders: map[string]string{"mcp:docs": "cage-DDDDDDDDDDDDDDDDDDDD"}}
	if err := st.SaveSandbox(sbx); err != nil {
		t.Fatal(err)
	}
	services, _ := m.servicesFor(sbx)
	sbx, err := m.prepareRemote(context.Background(), sbx, services)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := st.Sandbox(sbx.ID)
	if fmt.Sprint(saved.MCPPermissions) != "map[docs:[search_docs]]" || fmt.Sprint(sbx.MCPPermissions) != "map[docs:[search_docs]]" ||
		saved.MCPGrantURLs["docs"] != "https://docs.example.com/mcp" {
		t.Fatalf("the read-only default must be pinned, with its address, once the tools are known: %+v %v", saved, sbx.MCPPermissions)
	}
	if listed, _ := st.MCPService("docs"); len(listed.Catalog) != 2 || listed.CatalogAt == nil {
		t.Fatalf("catalog not saved: %+v", listed)
	}
	// The service adds a read-only tool; listing it again must not widen the pinned grant.
	tools.Store(`[{"name":"search_docs","annotations":{"readOnlyHint":true}},{"name":"delete_page"},{"name":"export_all","annotations":{"readOnlyHint":true}}]`)
	service, _ := st.MCPService("docs")
	if _, err := m.ListTools(context.Background(), service); err != nil {
		t.Fatal(err)
	}
	config, err := m.gateConfig(context.Background(), sbx, nil, nil)
	if err != nil || len(config.Connectors) != 1 || fmt.Sprint(config.Connectors[0].Tools) != "[search_docs]" || config.Connectors[0].Token != "" {
		t.Fatalf("a new tool must stay closed until the user grants it: %+v %v", config.Connectors, err)
	}
	current := sandbox.Sandbox{MCPPermissions: map[string][]string{"docs": {"search_docs", "gone_tool"}},
		MCPGrantURLs: map[string]string{"docs": "https://docs.example.com/mcp"}}
	permissions, urls, err := m.normalizeMCPPermissions([]string{"docs"}, nil, current)
	if err != nil || fmt.Sprint(permissions) != "map[docs:[search_docs]]" || urls["docs"] != "https://docs.example.com/mcp" {
		t.Fatalf("existing grants are kept, minus tools the service no longer offers: %v %v %v", permissions, urls, err)
	}
	if _, _, err := m.normalizeMCPPermissions([]string{"docs"}, map[string][]string{"docs": {"rm_rf"}}, sandbox.Sandbox{}); err == nil {
		t.Fatal("a requested tool the service does not offer must be refused")
	}
	fresh := mcp.Service{ID: "fresh", Name: "Fresh", URL: "https://docs.example.com/mcp", KeyRef: mcp.KeyRefFor("fresh"), Auth: mcp.AuthNone}
	if err := st.SaveMCPService(fresh); err != nil {
		t.Fatal(err)
	}
	permissions, _, err = m.normalizeMCPPermissions([]string{"fresh"}, nil, sandbox.Sandbox{})
	if _, pinned := permissions["fresh"]; err != nil || pinned {
		t.Fatalf("a service whose tools are unknown is pinned later, not with an empty grant: %v %v", permissions, err)
	}
}

// A grant is bound to the address it was made for: however a service is moved (the app, cagectl,
// or an edit racing a start), the new server never inherits the old server's grant.
func TestRemoteGrantDoesNotFollowTheServiceToAnotherAddress(t *testing.T) {
	var tools atomic.Value
	tools.Store(`[{"name":"search","annotations":{"readOnlyHint":true}},{"name":"update_page"}]`)
	m, st, _ := newRemoteManager(t, mcpServer(t, &tools))
	now := time.Now().UTC()
	docs := mcp.Service{ID: "docs", Name: "Docs", URL: "https://docs.example.com/mcp", KeyRef: mcp.KeyRefFor("docs"), Auth: mcp.AuthNone,
		Catalog: []mcp.Tool{{Name: "search", ReadOnly: true}, {Name: "update_page"}}, CatalogAt: &now}
	if err := st.SaveMCPService(docs); err != nil {
		t.Fatal(err)
	}
	sbx := sandbox.Sandbox{ID: "sbx-remote03", Name: "r", ProjectPath: "/Users/u/CageWorkspace/p", MCP: []string{"docs"},
		Bindings: []sandbox.Binding{{Agent: session.Codex}}, Placeholders: map[string]string{"mcp:docs": "cage-DDDDDDDDDDDDDDDDDDDD"},
		MCPPermissions: map[string][]string{"docs": {"search", "update_page"}}, MCPGrantURLs: map[string]string{"docs": "https://docs.example.com/mcp"}}
	if err := st.SaveSandbox(sbx); err != nil {
		t.Fatal(err)
	}
	// Moved without touching grants, as `cagectl mcp add` with the same ID used to do. The new
	// server lists the same names, but only marks search read-only.
	moved := docs
	moved.URL, moved.Catalog, moved.CatalogAt = "https://other.example.com/mcp", []mcp.Tool{{Name: "search"}, {Name: "update_page"}}, &now
	if err := st.SaveMCPService(moved); err != nil {
		t.Fatal(err)
	}
	if tools, explicit := Grant(sbx, moved); explicit || len(tools) != 0 {
		t.Fatalf("a grant for the old address must not apply: %v %v", tools, explicit)
	}
	config, err := m.gateConfig(context.Background(), sbx, nil, nil)
	if err != nil || len(config.Connectors) != 1 || config.Connectors[0].Upstream != "https://other.example.com/mcp" || len(config.Connectors[0].Tools) != 0 {
		t.Fatalf("the moved service starts with its own default, here nothing: %+v %v", config.Connectors, err)
	}
	services, _ := m.servicesFor(sbx)
	pinned, err := m.prepareRemote(context.Background(), sbx, services)
	if err != nil || pinned.MCPGrantURLs["docs"] != "https://other.example.com/mcp" || len(pinned.MCPPermissions["docs"]) != 0 {
		t.Fatalf("the new address gets its own pinned default: %+v %v", pinned, err)
	}
	// A tool list fetched from the old address is discarded once the service has moved.
	if _, err := SaveCatalog(st, docs, []mcp.Tool{{Name: "search", ReadOnly: true}}, time.Now()); !errors.Is(err, ErrServiceChanged) {
		t.Fatalf("a stale tool list must not overwrite the edit: %v", err)
	}
	if current, _ := st.MCPService("docs"); current.URL != "https://other.example.com/mcp" || current.Catalog[0].ReadOnly {
		t.Fatalf("the edit must survive: %+v", current)
	}
	if err := m.checkServices(make([]string, MaxMCPServices+1)); err == nil {
		t.Fatal("more MCP services than the gatekeeper routes must be refused up front")
	}
}
