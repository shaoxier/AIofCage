package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
	"github.com/shaoxier/AIofCage/internal/store"
	"github.com/shaoxier/AIofCage/internal/vault"
)

type memoryVault map[string][]byte

func (m memoryVault) Put(_ context.Context, ref string, value []byte) error {
	m[ref] = append([]byte(nil), value...)
	return nil
}
func (m memoryVault) Read(_ context.Context, ref string) ([]byte, error) {
	if value, ok := m[ref]; ok {
		return value, nil
	}
	return nil, vault.ErrNotFound
}
func (m memoryVault) Delete(_ context.Context, ref string) error { delete(m, ref); return nil }

func newApp(t *testing.T) (*App, memoryVault, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secrets := memoryVault{}
	return NewApp(st, secrets, nil, errors.New("测试环境没有 Docker"), nil), secrets, st
}

func codexRequest() SaveProviderRequest {
	return SaveProviderRequest{Name: "七牛 xapi", Agent: session.Codex, Protocol: provider.OpenAIResponses,
		BaseURL: "https://xapi.qiniu.io/v1", Model: "gpt-6.1-sol", Auth: provider.AuthBearer, APIKey: "secret-key-123"}
}

func TestSaveProviderStoresKeyOnlyInVault(t *testing.T) {
	app, secrets, st := newApp(t)
	view, err := app.SaveProvider(codexRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !view.KeySaved || !view.KeyManaged || view.KeyRef != provider.KeyRefFor(view.ID) || string(secrets[view.KeyRef]) != "secret-key-123" {
		t.Fatalf("unexpected view %+v", view)
	}
	items, _ := app.ListProviders()
	for _, item := range items {
		if strings.Contains(string(mustJSON(t, item)), "secret-key-123") {
			t.Fatal("provider view leaked the key")
		}
	}
	saved, _ := st.Provider(view.ID)
	if strings.Contains(string(mustJSON(t, saved)), "secret-key-123") {
		t.Fatal("store contains the key")
	}
	// Editing without a new key keeps the existing one.
	edit := codexRequest()
	edit.ID, edit.APIKey, edit.Model = view.ID, "", "gpt-6-sol"
	if view, err = app.SaveProvider(edit); err != nil || view.Model != "gpt-6-sol" || string(secrets[view.KeyRef]) != "secret-key-123" {
		t.Fatalf("edit: %+v %v", view, err)
	}
}

func TestSaveProviderValidation(t *testing.T) {
	app, secrets, _ := newApp(t)
	noKey := codexRequest()
	noKey.APIKey = ""
	if _, err := app.SaveProvider(noKey); err == nil {
		t.Fatal("new provider without key accepted")
	}
	foreign := codexRequest()
	foreign.APIKey, foreign.ExistingKeyRef = "", "github.com"
	if _, err := app.SaveProvider(foreign); err == nil {
		t.Fatal("arbitrary keychain item accepted")
	}
	secrets["sbx-builderday-key"] = []byte("competition")
	preset := codexRequest()
	preset.APIKey, preset.ExistingKeyRef = "", "sbx-builderday-key"
	view, err := app.SaveProvider(preset)
	if err != nil || view.KeyManaged || view.KeyRef != "sbx-builderday-key" {
		t.Fatalf("preset keychain item: %+v %v", view, err)
	}
	chat := codexRequest()
	chat.Protocol = provider.OpenAIChat
	if _, err := app.SaveProvider(chat); err == nil {
		t.Fatal("codex with chat protocol accepted")
	}
}

func TestPresetKeyStaysOnPresetHost(t *testing.T) {
	app, secrets, st := newApp(t)
	secrets["sbx-builderday-key"] = []byte("competition-key-0001")
	preset := codexRequest()
	preset.APIKey, preset.ExistingKeyRef = "", "sbx-builderday-key"
	view, err := app.SaveProvider(preset)
	if err != nil {
		t.Fatal(err)
	}
	// A copy refers to the preset item instead of cloning the key into an item of its own.
	copied, err := app.DuplicateProvider(view.ID)
	if err != nil || copied.KeyRef != "sbx-builderday-key" || len(secrets) != 1 {
		t.Fatalf("copy: %+v %v, %d vault items", copied, err, len(secrets))
	}
	moved := preset
	moved.ID, moved.BaseURL = copied.ID, "https://collector.example/v1"
	if _, err := app.SaveProvider(moved); err == nil || !strings.Contains(err.Error(), "xapi.qiniu.io") {
		t.Fatalf("preset key followed the provider to another host: %v", err)
	}
	// A record edited before this check existed is still refused when the key would be used.
	p, _ := st.Provider(view.ID)
	p.BaseURL = "https://collector.example/v1"
	if err := st.SaveProvider(p); err != nil {
		t.Fatal(err)
	}
	if result, err := app.TestProvider(view.ID); err != nil || result.OK || !strings.Contains(result.Message, "xapi.qiniu.io") {
		t.Fatalf("connection test used the preset key on another host: %+v %v", result, err)
	}
}

func TestProviderTestDoesNotFollowRedirects(t *testing.T) {
	app, _, _ := newApp(t)
	var followed atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/collect" {
			followed.Store(true)
			return
		}
		http.Redirect(w, r, "/collect", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	address := server.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (netConn, error) {
		return (&dialer{}).DialContext(ctx, network, address)
	}
	client := *app.client // keep the redirect policy NewApp sets
	client.Transport = transport
	app.client = &client
	view, err := app.SaveProvider(SaveProviderRequest{Name: "test", Agent: session.Codex, Protocol: provider.OpenAIResponses,
		BaseURL: "https://example.com/v1", Model: "m", Auth: provider.AuthXAPIKey, APIKey: "redirect-test-key"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.TestProvider(view.ID)
	if err != nil || result.OK || result.Status != http.StatusTemporaryRedirect || !strings.Contains(result.Message, "跳转") || followed.Load() {
		t.Fatalf("redirect: %+v %v followed=%v", result, err, followed.Load())
	}
}

func TestUpdatingKeyCannotOverwriteAppliedCredential(t *testing.T) {
	app, secrets, st := newApp(t)
	view, err := app.SaveProvider(codexRequest())
	if err != nil {
		t.Fatal(err)
	}
	p, _ := st.Provider(view.ID)
	s := sandbox.Sandbox{ID: "sbx-active", Name: "active", AppliedProviders: map[string]provider.Provider{p.ID: p}}
	if err := st.SaveSandbox(s); err != nil {
		t.Fatal(err)
	}
	edit := codexRequest()
	edit.ID, edit.APIKey = p.ID, "replacement-key"
	if _, err := app.SaveProvider(edit); err == nil || !strings.Contains(err.Error(), "先停止沙箱") {
		t.Fatalf("覆盖了已生效密钥：%v", err)
	}
	if string(secrets[p.KeyRef]) != "secret-key-123" {
		t.Fatal("拒绝更新后原密钥发生变化")
	}
	s.AppliedProviders = nil
	if err := st.SaveSandbox(s); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SaveProvider(edit); err != nil || string(secrets[p.KeyRef]) != "replacement-key" {
		t.Fatalf("停止后未能更新密钥：%v", err)
	}
}

func TestDeleteProviderInUse(t *testing.T) {
	app, secrets, st := newApp(t)
	view, err := app.SaveProvider(codexRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSandbox(sandbox.Sandbox{ID: "sbx-1", Name: "demo", Bindings: []sandbox.Binding{{Agent: session.Codex, ProviderID: view.ID}}}); err != nil {
		t.Fatal(err)
	}
	if err := app.DeleteProvider(view.ID); err == nil || !strings.Contains(err.Error(), "demo") {
		t.Fatalf("in-use provider deleted: %v", err)
	}
	edit := codexRequest()
	edit.ID, edit.APIKey, edit.Agent, edit.Protocol = view.ID, "", session.ClaudeCode, provider.Anthropic
	if _, err := app.SaveProvider(edit); err == nil {
		t.Fatal("changed agent of an in-use provider")
	}
	st.DeleteSandbox("sbx-1")
	if err := app.DeleteProvider(view.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := secrets[view.KeyRef]; ok {
		t.Fatal("managed key not removed from vault")
	}
}

func TestTestProviderClassifiesResults(t *testing.T) {
	app, _, _ := newApp(t)
	var gotAuth string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if gotAuth != "Bearer good-key" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()
	// Providers must use a DNS name; httptest's certificate covers example.com, so route that
	// name to the test server. This override exists only in the test.
	transport := server.Client().Transport.(*http.Transport).Clone()
	address := server.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (netConn, error) {
		return (&dialer{}).DialContext(ctx, network, address)
	}
	app.client = &http.Client{Transport: transport}
	request := SaveProviderRequest{Name: "test", Agent: session.Codex, Protocol: provider.OpenAIResponses,
		BaseURL: "https://example.com/v1", Model: "m", Auth: provider.AuthBearer, APIKey: "good-key"}
	view, err := app.SaveProvider(request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.TestProvider(view.ID)
	if err != nil || !result.OK || result.Status != 200 || gotAuth != "Bearer good-key" {
		t.Fatalf("good key: %+v %v", result, err)
	}
	request.ID, request.APIKey = view.ID, "wrong-key"
	if _, err := app.SaveProvider(request); err != nil {
		t.Fatal(err)
	}
	if result, _ = app.TestProvider(view.ID); result.OK || result.Status != 401 {
		t.Fatalf("bad key: %+v", result)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := jsonMarshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
