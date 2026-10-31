package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/session"
)

// modelServer answers every host the app contacts; it records the requests it saw.
type modelServer struct {
	mu       sync.Mutex
	requests []*http.Request
	calls    atomic.Int32
}

func (s *modelServer) seen() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.requests...)
}

func serveModels(t *testing.T, app *App, handler http.HandlerFunc) *modelServer {
	t.Helper()
	recorded := &modelServer{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorded.calls.Add(1)
		recorded.mu.Lock()
		recorded.requests = append(recorded.requests, r.Clone(context.Background()))
		recorded.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport).Clone()
	address := server.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (netConn, error) {
		return (&dialer{}).DialContext(ctx, network, address)
	}
	client := *app.client // keep the redirect policy NewApp sets
	client.Transport = transport
	app.client = &client
	return recorded
}

func TestModelListIsFilteredCachedAndRefreshed(t *testing.T) {
	app, _, _ := newApp(t)
	server := serveModels(t, app, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"gpt-6.1-sol"},{"id":"gpt-6-sol"},{"id":"gpt-6-sol"},{"id":"<script>"},{"id":""},{"id":"a b"},{"id":"deepseek-ai/DeepSeek-V3"},{"id":"@cf/meta/llama-3-8b-instruct"}]}`))
	})
	view, err := app.SaveProvider(SaveProviderRequest{Name: "xapi", Agent: session.Codex, Protocol: provider.OpenAIResponses,
		BaseURL: "https://xapi.example.com/v1", Model: "gpt-6.1-sol", Auth: provider.AuthBearer, APIKey: "synthetic-models-key-0123"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := app.ProviderModels(view.ID, false)
	if err != nil || !list.OK || strings.Join(list.Models, ",") != "@cf/meta/llama-3-8b-instruct,deepseek-ai/DeepSeek-V3,gpt-6-sol,gpt-6.1-sol" || list.Cached {
		t.Fatalf("got %+v %v", list, err)
	}
	if got := server.seen()[0]; got.URL.Path != "/v1/models" || got.Header.Get("Authorization") != "Bearer synthetic-models-key-0123" {
		t.Fatalf("request %s %v", got.URL, got.Header)
	}
	if again, _ := app.ProviderModels(view.ID, false); !again.Cached || server.calls.Load() != 1 {
		t.Fatalf("the second look must come from the cache: %+v, %d calls", again, server.calls.Load())
	}
	if refreshed, _ := app.ProviderModels(view.ID, true); refreshed.Cached || server.calls.Load() != 2 {
		t.Fatalf("a refresh asks again: %+v, %d calls", refreshed, server.calls.Load())
	}
	request := SaveProviderRequest{ID: view.ID, Name: "xapi", Agent: session.Codex, Protocol: provider.OpenAIResponses,
		BaseURL: "https://other.example.com/v1", Model: "gpt-6.1-sol", Auth: provider.AuthBearer}
	if _, err := app.SaveProvider(request); err != nil {
		t.Fatal(err)
	}
	if after, _ := app.ProviderModels(view.ID, false); after.Cached || server.calls.Load() != 3 {
		t.Fatalf("saving the provider drops its cached list: %+v", after)
	}
}

func TestAnthropicModelListIsPaged(t *testing.T) {
	app, _, _ := newApp(t)
	server := serveModels(t, app, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after_id") == "" {
			w.Write([]byte(`{"data":[{"id":"claude-a"}],"has_more":true,"last_id":"claude-a"}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"claude-b"}],"has_more":false,"last_id":"claude-b"}`))
	})
	view, err := app.SaveProvider(SaveProviderRequest{Name: "anthropic", Agent: session.ClaudeCode, Protocol: provider.Anthropic,
		BaseURL: "https://api.example.com", Model: "claude-a", Auth: provider.AuthXAPIKey, APIKey: "synthetic-anthropic-key-0123"})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := app.ProviderModels(view.ID, false)
	requests := server.seen()
	if strings.Join(list.Models, ",") != "claude-a,claude-b" || len(requests) != 2 {
		t.Fatalf("got %+v after %d requests", list, len(requests))
	}
	first, second := requests[0], requests[1]
	if first.URL.Path != "/v1/models" || first.URL.Query().Get("limit") != "1000" || first.Header.Get("X-Api-Key") != "synthetic-anthropic-key-0123" ||
		first.Header.Get("Anthropic-Version") == "" || second.URL.Query().Get("after_id") != "claude-a" {
		t.Fatalf("requests %s %v / %s", first.URL, first.Header, second.URL)
	}
}

func TestModelListFailuresAreExplained(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusUnauthorized, `{"error":"bad key"}`, "密钥无效"},
		{http.StatusNotFound, ``, "不提供模型列表接口"},
		{http.StatusOK, `{"data":[]}`, "为空"},
		{http.StatusOK, `not json`, "无法识别"},
		{http.StatusTemporaryRedirect, ``, "跳转"},
	} {
		app, _, _ := newApp(t)
		server := serveModels(t, app, func(w http.ResponseWriter, r *http.Request) {
			if c.status == http.StatusTemporaryRedirect {
				http.Redirect(w, r, "https://collect.example.com/models", c.status)
				return
			}
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		})
		view, err := app.SaveProvider(SaveProviderRequest{Name: "p", Agent: session.Codex, Protocol: provider.OpenAIResponses,
			BaseURL: "https://api.example.com/v1", Model: "m", Auth: provider.AuthBearer, APIKey: "synthetic-failure-key-0123"})
		if err != nil {
			t.Fatal(err)
		}
		list, _ := app.ProviderModels(view.ID, false)
		if list.OK || len(list.Models) != 0 || !strings.Contains(list.Message, c.want) || server.calls.Load() != 1 {
			t.Errorf("%d %q: %+v after %d calls", c.status, c.body, list, server.calls.Load())
		}
		// A service without a list endpoint is remembered; other failures are asked again.
		if again, _ := app.ProviderModels(view.ID, false); again.Cached != (c.status == http.StatusNotFound) {
			t.Errorf("%d: cached=%v", c.status, again.Cached)
		}
	}
}

func TestPreviewBeforeSaving(t *testing.T) {
	app, secrets, _ := newApp(t)
	server := serveModels(t, app, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	})
	typed := SaveProviderRequest{Name: "new", Agent: session.Codex, Protocol: provider.OpenAIResponses,
		BaseURL: "https://api.example.com/v1", Auth: provider.AuthBearer, APIKey: " synthetic-typed-key-0123 "}
	if list, err := app.PreviewProviderModels(typed); err != nil || !list.OK || list.Models[0] != "m1" ||
		server.seen()[0].Header.Get("Authorization") != "Bearer synthetic-typed-key-0123" {
		t.Fatalf("typed key: %+v %v", list, err)
	}
	// A preset's Keychain item goes only to the preset's host, even before saving.
	var bound provider.Preset
	for _, preset := range provider.Presets() {
		if preset.ExistingKeyRef != "" {
			bound = preset
		}
	}
	secrets[bound.ExistingKeyRef] = []byte("synthetic-preset-key-0123")
	elsewhere := SaveProviderRequest{Name: "p", Agent: bound.Agent, Protocol: bound.Protocol, BaseURL: "https://elsewhere.example.com/v1",
		Auth: bound.Auth, Preset: bound.ID, ExistingKeyRef: bound.ExistingKeyRef}
	calls := server.calls.Load()
	if list, err := app.PreviewProviderModels(elsewhere); err != nil || list.OK || server.calls.Load() != calls {
		t.Fatalf("the preset key reached another host: %+v %v", list, err)
	}
	if list, _ := app.PreviewProviderModels(SaveProviderRequest{Name: "p", Agent: session.Codex, Protocol: provider.OpenAIResponses,
		BaseURL: "https://api.example.com/v1", Auth: provider.AuthBearer}); list.OK || !strings.Contains(list.Message, "API Key") {
		t.Fatalf("without a key: %+v", list)
	}
}

// Every name the list offers can be saved.
func TestListedModelNamesCanBeSaved(t *testing.T) {
	app, _, _ := newApp(t)
	for _, model := range []string{"@cf/meta/llama-3-8b-instruct", "org/model+beta", "qwen:72b", strings.Repeat("m", 200)} {
		if _, err := app.SaveProvider(SaveProviderRequest{Name: "p", Agent: session.Codex, Protocol: provider.OpenAIResponses,
			BaseURL: "https://api.example.com/v1", Model: model, Auth: provider.AuthBearer, APIKey: "synthetic-save-key-0123"}); err != nil {
			t.Errorf("%s: %v", model, err)
		}
	}
}

// A list fetched with a key that changed meanwhile is not stored.
func TestListFetchedBeforeAKeyChangeIsNotCached(t *testing.T) {
	app, _, _ := newApp(t)
	asked, release := make(chan struct{}, 1), make(chan struct{})
	serveModels(t, app, func(w http.ResponseWriter, r *http.Request) {
		asked <- struct{}{}
		<-release
		w.Write([]byte(`{"data":[{"id":"listed-with-old-key"}]}`))
	})
	view, err := app.SaveProvider(SaveProviderRequest{Name: "p", Agent: session.Codex, Protocol: provider.OpenAIResponses,
		BaseURL: "https://api.example.com/v1", Model: "m", Auth: provider.AuthBearer, APIKey: "synthetic-old-key-0123"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan ModelList, 1)
	go func() { list, _ := app.ProviderModels(view.ID, true); done <- list }()
	<-asked
	app.models.forget(view.KeyRef) // the key is replaced while the request is out
	close(release)
	if list := <-done; !list.OK {
		t.Fatalf("the fetch itself failed: %+v", list)
	}
	if _, ok := app.models.get(modelCacheKey(view.Provider), app.now()); ok {
		t.Fatal("a list fetched with the old key was cached")
	}
}

// A later page that fails keeps what earlier pages listed.
func TestPartialAnthropicListIsKept(t *testing.T) {
	app, _, _ := newApp(t)
	serveModels(t, app, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after_id") == "" {
			w.Write([]byte(`{"data":[{"id":"claude-a"}],"has_more":true,"last_id":"claude-a"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	view, err := app.SaveProvider(SaveProviderRequest{Name: "a", Agent: session.ClaudeCode, Protocol: provider.Anthropic,
		BaseURL: "https://api.example.com", Model: "claude-a", Auth: provider.AuthXAPIKey, APIKey: "synthetic-partial-key-0123"})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := app.ProviderModels(view.ID, false)
	if !list.OK || strings.Join(list.Models, ",") != "claude-a" || !strings.Contains(list.Message, "没有读完") {
		t.Fatalf("got %+v", list)
	}
}

// A preview reaches a provider's own Keychain item only through the saved provider.
func TestPreviewRefusesAnUnsavedProvidersItem(t *testing.T) {
	app, secrets, _ := newApp(t)
	server := serveModels(t, app, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"data":[{"id":"m"}]}`)) })
	secrets[provider.KeyRefFor("p-orphan")] = []byte("synthetic-orphan-key-0123")
	_, err := app.PreviewProviderModels(SaveProviderRequest{ID: "p-orphan", Name: "p", Agent: session.Codex, Protocol: provider.OpenAIResponses,
		BaseURL: "https://elsewhere.example.com/v1", Auth: provider.AuthBearer, ExistingKeyRef: provider.KeyRefFor("p-orphan")})
	if err == nil || server.calls.Load() != 0 {
		t.Fatalf("an orphaned item was sent: %v, %d calls", err, server.calls.Load())
	}
}
