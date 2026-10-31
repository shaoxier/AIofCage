package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/gatekeeper"
	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/policy"
	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
)

// liveGate is a real gatekeeper behind a running sandbox's management client. older makes it
// refuse fields that a gatekeeper from before development sources does not know, as such a
// gatekeeper answers.
type liveGate struct {
	mu    sync.Mutex
	older bool
	rules []policy.Rule // rules of the last configuration the gatekeeper took
	taken int
}

func (g *liveGate) client(t *testing.T) *gateClient {
	t.Helper()
	token := "c3ludGhldGljLW1hbmFnZW1lbnQtdG9rZW4tMDEyMzQ1Njc4OWFiY2RlZg"
	handlers, err := gatekeeper.NewHandlers(token)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/config" {
			handlers.Management.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.older && strings.Contains(string(body), `"anonymous"`) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"code":"invalid_json"}`)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		handlers.Management.ServeHTTP(recorder, r)
		if recorder.Code == http.StatusOK {
			var config gatekeeper.Config
			_ = json.Unmarshal(body, &config)
			g.rules, g.taken = config.Policy.Rules, g.taken+1
		}
		w.WriteHeader(recorder.Code)
		w.Write(recorder.Body.Bytes())
	}))
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	return newGateClient(u.Port(), token)
}

func (g *liveGate) opens(host string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, rule := range g.rules {
		if rule.Effect == policy.Allow && rule.Host == host {
			return true
		}
	}
	return false
}

func (g *liveGate) empty() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.taken > 0 && len(g.rules) == 0
}

// fakeDocker answers like Docker for a sandbox whose agent runs and logs every command. gate is
// what it says about the gatekeeper container: "running", "stopped", or "silent" for a Docker that
// does not respond.
func fakeDocker(t *testing.T, gate string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "commands")
	answer := map[string]string{"running": "echo 'true img spec'", "stopped": "echo 'false img spec'",
		"silent": "echo 'Cannot connect to the Docker daemon. Is the docker daemon running?' >&2; exit 1"}[gate]
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\ncase \"$*\" in\n" +
		"*'container inspect'*-gate) " + answer + ";;\n*'container inspect'*) echo 'true img spec';;\nesac\nexit 0\n"
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, log
}

// liveSandbox is a running sandbox with a Codex provider whose key is in the vault.
func liveSandbox(t *testing.T, gate *liveGate) (*Manager, memoryVault, string) {
	t.Helper()
	m, st, vault := newRemoteManager(t, nil)
	docker, log := fakeDocker(t, "running")
	m.docker = runner{path: docker}
	m.gates["sbx-live0001"] = gate.client(t)
	vault["aiofcage-provider-p1"] = []byte("synthetic-provider-key-0123456789")
	if err := st.SaveProvider(provider.Provider{ID: "p1", Name: "P1", Agent: session.Codex, Protocol: provider.OpenAIResponses,
		BaseURL: "https://api.example.com/v1", Model: "m", Auth: provider.AuthBearer, KeyRef: "aiofcage-provider-p1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSandbox(sandbox.Sandbox{ID: "sbx-live0001", Name: "live", ProjectPath: "/Users/u/CageWorkspace/p",
		Bindings: []sandbox.Binding{{Agent: session.Codex, ProviderID: "p1"}}, Placeholders: map[string]string{"p1": "cage-PPPPPPPPPPPPPPPPPPPP"}}); err != nil {
		t.Fatal(err)
	}
	return m, vault, log
}

// Settings are saved before they are delivered; a gatekeeper that missed them could keep a source
// open that the user closed, so the sandbox goes offline until a refresh succeeds.
func TestMissedSettingsTakeTheSandboxOffline(t *testing.T) {
	gate := &liveGate{}
	m, vault, _ := liveSandbox(t, gate)
	ctx := context.Background()
	if err := m.SetDevSources(ctx, "sbx-live0001", []string{"github"}); err != nil || !gate.opens("github.com") {
		t.Fatalf("opening GitHub: %v %v", err, gate.rules)
	}
	delete(vault, "aiofcage-provider-p1") // e.g. the Keychain prompt was refused
	err := m.SetDevSources(ctx, "sbx-live0001", nil)
	if err == nil || !strings.Contains(err.Error(), "已暂停这个沙箱的全部网络访问") || !gate.empty() {
		t.Fatalf("closing GitHub failed to deliver, yet the gatekeeper kept rules: %v %v", err, gate.rules)
	}
	stored, _ := m.store.Sandbox("sbx-live0001")
	if len(stored.DevSources) != 0 {
		t.Fatalf("the user's choice stays saved: %v", stored.DevSources)
	}
	vault["aiofcage-provider-p1"] = []byte("synthetic-provider-key-0123456789")
	if err := m.Refresh(ctx, "sbx-live0001"); err != nil || !gate.opens("api.example.com") || gate.opens("github.com") {
		t.Fatalf("a later refresh brings back the stored settings: %v %v", err, gate.rules)
	}
}

// A sandbox still running a gatekeeper from before development sources refuses the new rules. It
// must not keep its old configuration, or later revocations (MCP tools, OAuth logins) would never
// reach it.
func TestOlderGatekeeperGoesOfflineInsteadOfKeepingOldRules(t *testing.T) {
	gate := &liveGate{}
	m, _, _ := liveSandbox(t, gate)
	ctx := context.Background()
	if err := m.Refresh(ctx, "sbx-live0001"); err != nil || !gate.opens("api.example.com") {
		t.Fatalf("first delivery: %v", err)
	}
	gate.mu.Lock()
	gate.older = true
	gate.mu.Unlock()
	err := m.SetDevSources(ctx, "sbx-live0001", []string{"npm"})
	if err == nil || !strings.Contains(err.Error(), "旧版门卫") || !strings.Contains(err.Error(), "已暂停") || !gate.empty() {
		t.Fatalf("an older gatekeeper must go offline: %v %v", err, gate.rules)
	}
	if err := m.Refresh(ctx, "sbx-live0001"); err == nil || !gate.empty() {
		t.Fatalf("every later refresh keeps it offline until a restart: %v %v", err, gate.rules)
	}
}

// When Docker cannot say whether the sandbox runs, it is treated as running: the settings still
// reach a gatekeeper that answers, and one that does not is stopped.
func TestUnknownStateIsTreatedAsRunning(t *testing.T) {
	gate := &liveGate{}
	m, _, _ := liveSandbox(t, gate)
	ctx := context.Background()
	if err := m.SetDevSources(ctx, "sbx-live0001", []string{"github"}); err != nil {
		t.Fatal(err)
	}
	docker, log := fakeDocker(t, "silent")
	m.docker = runner{path: docker}
	if err := m.SetDevSources(ctx, "sbx-live0001", nil); err != nil || gate.opens("github.com") || !gate.opens("api.example.com") {
		t.Fatalf("an unanswered inspect must not count as stopped: %v %v", err, gate.rules)
	}
	m.gates["sbx-live0001"] = newGateClient("1", "unreachable") // nothing listens on port 1
	err := m.SetDevSources(ctx, "sbx-live0001", []string{"npm"})
	if commands, _ := os.ReadFile(log); err == nil || !strings.Contains(string(commands), "stop --time 5 aiofcage-sbx-live0001-gate") {
		t.Fatalf("an unreachable gatekeeper of a sandbox that may run is stopped: %v\n%s", err, commands)
	}
}

// Docker not answering because the VM is stopped means nothing runs; any other VM state does not.
func TestStoppedVMMeansNothingRuns(t *testing.T) {
	for status, delivered := range map[string]bool{"Stopped": false, "Broken": true} {
		gate := &liveGate{}
		m, _, _ := liveSandbox(t, gate)
		docker, _ := fakeDocker(t, "silent")
		m.docker = runner{path: docker}
		colima := filepath.Join(t.TempDir(), "colima")
		script := "#!/bin/sh\necho '{\"name\":\"" + ColimaProfile + "\",\"status\":\"" + status + "\"}'\n"
		if err := os.WriteFile(colima, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		m.env = &Environment{colima: runner{path: colima}}
		err := m.SetDevSources(context.Background(), "sbx-live0001", []string{"github"})
		if err != nil || gate.opens("github.com") != delivered {
			t.Fatalf("VM %s: delivered=%v want %v (%v)", status, gate.opens("github.com"), delivered, err)
		}
	}
}

// A refresh that cannot wait for the sandbox's lock (its time ran out, or another operation holds
// the sandbox) still takes the sandbox offline when the change may narrow the settings.
func TestRefreshThatCannotWaitTakesTheSandboxOffline(t *testing.T) {
	gate := &liveGate{}
	m, _, _ := liveSandbox(t, gate)
	if err := m.SetDevSources(context.Background(), "sbx-live0001", []string{"github"}); err != nil {
		t.Fatal(err)
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.RefreshCredentials(expired, "sbx-live0001"); err == nil || gate.empty() {
		t.Fatalf("a credential refresh takes nothing away and keeps the gatekeeper: %v %v", err, gate.rules)
	}
	if err := m.Refresh(expired, "sbx-live0001"); err == nil || !strings.Contains(err.Error(), "已暂停") || !gate.empty() {
		t.Fatalf("a refresh that may narrow must not leave the old configuration: %v %v", err, gate.rules)
	}
}

// Renewed credentials that miss the gatekeeper leave its configuration in place: nothing was taken
// away, and the caller retries.
func TestMissedCredentialsKeepTheGatekeeper(t *testing.T) {
	gate := &liveGate{}
	m, vault, _ := liveSandbox(t, gate)
	if err := m.Refresh(context.Background(), "sbx-live0001"); err != nil {
		t.Fatal(err)
	}
	delete(vault, "aiofcage-provider-p1")
	if err := m.RefreshCredentials(context.Background(), "sbx-live0001"); err == nil || !gate.opens("api.example.com") {
		t.Fatalf("got %v %v", err, gate.rules)
	}
}

// A gatekeeper that accepts connections but never answers is stopped before closing runs out of time.
func TestHangingGatekeeperIsStopped(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		var held []net.Conn
		defer func() {
			for _, conn := range held {
				conn.Close()
			}
		}()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			held = append(held, conn) // never answer
		}
	}()
	m, _, log := liveSandbox(t, &liveGate{})
	m.gates["sbx-live0001"] = newGateClient(strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), "token")
	saved := clearTimeout
	clearTimeout = 2 * time.Second
	defer func() { clearTimeout = saved }()
	started := time.Now()
	err = m.closeGate(context.Background(), sandbox.Sandbox{ID: "sbx-live0001", Name: "live"}, errors.New("门卫没有回应"))
	commands, _ := os.ReadFile(log)
	if err == nil || !strings.Contains(err.Error(), "已停止它的门卫") || !strings.Contains(string(commands), "stop --time 5") || time.Since(started) > 10*time.Second {
		t.Fatalf("the gatekeeper must be stopped in time (%s): %v\n%s", time.Since(started), err, commands)
	}
}

// A delivery that ran out of time still takes the sandbox offline: closing has its own deadline.
func TestExpiredDeliveryStillTakesTheSandboxOffline(t *testing.T) {
	gate := &liveGate{}
	m, _, _ := liveSandbox(t, gate)
	if err := m.SetDevSources(context.Background(), "sbx-live0001", []string{"github"}); err != nil {
		t.Fatal(err)
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	sbx, _ := m.store.Sandbox("sbx-live0001")
	if err := m.refresh(expired, sbx, true); err == nil || !strings.Contains(err.Error(), "已暂停") || !gate.empty() {
		t.Fatalf("an expired delivery must still clear the gatekeeper: %v %v", err, gate.rules)
	}
}

// A gatekeeper whose management port does not answer is stopped, so the sandbox has no way out.
func TestUnreachableGatekeeperIsStopped(t *testing.T) {
	m, _, _ := liveSandbox(t, &liveGate{})
	docker, log := fakeDocker(t, "stopped")
	m.docker = runner{path: docker}
	m.gates["sbx-live0001"] = newGateClient("1", "unreachable") // nothing listens on port 1
	err := m.closeGate(context.Background(), sandbox.Sandbox{ID: "sbx-live0001", Name: "live"}, errors.New("门卫没有就绪"))
	commands, _ := os.ReadFile(log)
	if err == nil || !strings.Contains(err.Error(), "已停止它的门卫") || !strings.Contains(string(commands), "stop --time 5 aiofcage-sbx-live0001-gate") {
		t.Fatalf("the gatekeeper must be stopped: %v\n%s", err, commands)
	}
}

// A connector launches with the service as it is when it starts, not as the sandbox's settings
// were read minutes earlier: an edited database address comes with its own password.
func TestConnectorsLaunchWithTheServiceAsItIsNow(t *testing.T) {
	m, st, vault := newRemoteManager(t, nil)
	read := mcp.Service{ID: "shop", Name: "Shop", Kind: mcp.KindPostgres, URL: "postgresql://app@old.example/shop", KeyRef: mcp.KeyRefFor("shop")}
	edited := read
	edited.URL = "postgresql://app@new.example/shop"
	if err := st.SaveMCPService(edited); err != nil {
		t.Fatal(err)
	}
	vault[read.KeyRef] = []byte("synthetic-new-password")
	service, secret, err := m.connectorSecret(context.Background(), read)
	if err != nil || service.URL != edited.URL || string(secret) != "synthetic-new-password" {
		t.Fatalf("got %q %q %v", service.URL, secret, err)
	}
	if err := st.SaveMCPService(mcp.Service{ID: "shop", Name: "Shop", URL: "https://docs.example.com/mcp", KeyRef: read.KeyRef}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.connectorSecret(context.Background(), read); err == nil {
		t.Fatal("a service that is no longer a connector must not be launched")
	}
}
