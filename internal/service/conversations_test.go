package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
)

func TestConversationValidation(t *testing.T) {
	for _, options := range []ConversationOptions{{Model: ""}, {Model: "test\x1b[200~"}, {Model: "test", Effort: "invalid"}} {
		if validateConversationOptions(options) == nil {
			t.Fatalf("接受了非法选项：%+v", options)
		}
	}
	if err := validateConversationOptions(ConversationOptions{Model: "vendor/model", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	buffer := &tailBuffer{limit: 8}
	_, _ = buffer.Write([]byte(strings.Repeat("a", 100)))
	_, _ = buffer.Write([]byte("END"))
	if string(buffer.data) != "aaaaaEND" {
		t.Fatalf("诊断未按上限保存尾部：%q", buffer.data)
	}
	for _, tc := range []struct{ detail, want string }{
		{"401 unauthorized", "供应商认证失败"},
		{"429 rate limit", "供应商限流"},
		{"dial timeout", "请求超时"},
		{"connection refused", "无法连接供应商"},
	} {
		if got := classifyConversationFailure(tc.detail); !strings.Contains(got, tc.want) {
			t.Fatalf("%q: %q", tc.detail, got)
		}
	}
}

func TestConversationRecoveryMetadataRoundTrip(t *testing.T) {
	app, _, st := newApp(t)
	p := provider.Provider{ID: "p1", Name: "测试供应商", Agent: session.Codex}
	if err := st.SaveProvider(p); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSandbox(sandbox.Sandbox{ID: "sbx-recovery", Name: "可恢复", Bindings: []sandbox.Binding{{Agent: session.Codex, ProviderID: p.ID}}}); err != nil {
		t.Fatal(err)
	}
	app.now = func() time.Time { return time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC) }
	item := app.conversationRecoveryFrom(&conversation{id: "t-recovery", sandbox: sandbox.Sandbox{ID: "sbx-recovery", Bindings: []sandbox.Binding{{Agent: session.Codex, ProviderID: p.ID}}}, agent: session.Codex,
		provider: p, model: "model-a", effort: "high", nativeID: "native-session-1"})
	if err := app.saveConversationRecovery(item); err != nil {
		t.Fatal(err)
	}
	items, err := app.ListRecoverableConversations()
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %+v %v", items, err)
	}
	if items[0].ID != item.ID || items[0].SandboxName != "可恢复" || items[0].ProviderName != p.Name || !items[0].Ready {
		t.Fatalf("unexpected recovery view: %+v", items[0])
	}
	if raw := st.Setting(conversationRecoverySetting); strings.Contains(raw, "message") || !strings.Contains(raw, "native-session-1") {
		t.Fatalf("unexpected persisted metadata: %s", raw)
	}
	if err := app.removeConversationRecovery(item.ID); err != nil {
		t.Fatal(err)
	}
	if items, err := app.ListRecoverableConversations(); err != nil || len(items) != 0 {
		t.Fatalf("remove: %+v %v", items, err)
	}
}

// Claude Code repeats its session id on every event: only a new id is written, and each write
// tells the interface to read the recoverable list again.
func TestNativeSessionIsWrittenOnlyWhenItChanges(t *testing.T) {
	app, _, st := newApp(t)
	p := provider.Provider{ID: "p1", Name: "测试供应商", Agent: session.ClaudeCode}
	st.SaveProvider(p)
	sbx := sandbox.Sandbox{ID: "sbx-native", Name: "native", Bindings: []sandbox.Binding{{Agent: session.ClaudeCode, ProviderID: p.ID}}}
	st.SaveSandbox(sbx)
	notices := 0
	app.emit = func(event string, _ any) {
		if event == "conversation:recovery" {
			notices++
		}
	}
	c := &conversation{id: "t-native", sandbox: sbx, agent: session.ClaudeCode, provider: p}
	for _, id := range []string{"session-a", "session-a", "session-a", "", "session-b"} {
		app.noteNativeSession(c, id)
	}
	items, _ := app.recoveryRecords()
	if notices != 2 || items["t-native"].NativeSessionID != "session-b" {
		t.Fatalf("%d writes, record %+v", notices, items["t-native"])
	}
	c.closed = true
	if app.noteNativeSession(c, "session-c") {
		t.Fatal("a closed conversation must not write")
	}
}

// Records of a deleted sandbox or provider go when the list is read: a provider cannot be deleted
// while bound, so its conversations never continue.
func TestRecoveryRecordsOfDeletedSandboxesAndProvidersAreDropped(t *testing.T) {
	app, _, st := newApp(t)
	p := provider.Provider{ID: "p1", Name: "测试供应商", Agent: session.Codex}
	st.SaveProvider(p)
	sbx := sandbox.Sandbox{ID: "sbx-kept", Name: "kept", Bindings: []sandbox.Binding{{Agent: session.Codex, ProviderID: p.ID}}}
	st.SaveSandbox(sbx)
	for _, item := range []conversationRecovery{
		{ID: "t-kept", SandboxID: "sbx-kept", Agent: session.Codex, ProviderID: p.ID, NativeSessionID: "native-1"},
		{ID: "t-gone", SandboxID: "sbx-deleted", Agent: session.Codex, ProviderID: p.ID, NativeSessionID: "native-2"},
		{ID: "t-orphan", SandboxID: "sbx-kept", Agent: session.Codex, ProviderID: "p-deleted", NativeSessionID: "native-3"},
	} {
		if err := app.saveConversationRecovery(item); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := app.ListRecoverableConversations()
	items, _ := app.recoveryRecords()
	if err != nil || len(listed) != 1 || listed[0].ID != "t-kept" || len(items) != 1 {
		t.Fatalf("listed %+v, stored %+v, %v", listed, items, err)
	}
}

// A record whose native session is gone from the sandbox is removed before anything starts.
func TestRecoveryWithoutTheNativeSessionIsRemoved(t *testing.T) {
	app, _, st := newApp(t)
	app.manager = &runtime.Manager{} // only so the runtime counts as ready; nothing reaches it
	p := provider.Provider{ID: "p1", Name: "测试供应商", Agent: session.Codex}
	st.SaveProvider(p)
	st.SaveSandbox(sandbox.Sandbox{ID: "sbx-reset", Name: "reset", Bindings: []sandbox.Binding{{Agent: session.Codex, ProviderID: p.ID}}})
	if err := app.saveConversationRecovery(conversationRecovery{ID: "t-old", SandboxID: "sbx-reset", Agent: session.Codex, ProviderID: p.ID, NativeSessionID: "native-old"}); err != nil {
		t.Fatal(err)
	}
	checked := ""
	app.nativeSessionExists = func(_ context.Context, sandboxID string, agent session.Agent, nativeID string) (bool, error) {
		checked = sandboxID + "/" + string(agent) + "/" + nativeID
		return false, nil
	}
	_, err := app.RecoverConversation("t-old")
	items, _ := app.recoveryRecords()
	if err == nil || !strings.Contains(err.Error(), "已移除这条恢复记录") || len(items) != 0 || checked != "sbx-reset/codex/native-old" {
		t.Fatalf("got %v, records %+v, checked %q", err, items, checked)
	}
}

// Records stay when the configuration cannot be read for a moment: unreadable, or written by
// another build.
func TestRecoveryRecordsSurviveAnUnreadableConfiguration(t *testing.T) {
	app, _, st := newApp(t)
	p := provider.Provider{ID: "p1", Name: "测试供应商", Agent: session.Codex}
	st.SaveProvider(p)
	st.SaveSandbox(sandbox.Sandbox{ID: "sbx-live", Name: "live", Bindings: []sandbox.Binding{{Agent: session.Codex, ProviderID: p.ID}}})
	if err := app.saveConversationRecovery(conversationRecovery{ID: "t-live", SandboxID: "sbx-live", Agent: session.Codex, ProviderID: p.ID, NativeSessionID: "native-1"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(st.Dir(), "sandboxes.json")
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, damage := range map[string]func() error{
		"unreadable": func() error { return os.Chmod(path, 0o000) },
		"damaged":    func() error { return os.WriteFile(path, []byte(`[{"id": 42}]`), 0o600) },
	} {
		if err := damage(); err != nil {
			t.Fatal(err)
		}
		_, listErr := app.ListRecoverableConversations()
		os.Chmod(path, 0o600)
		if err := os.WriteFile(path, good, 0o600); err != nil {
			t.Fatal(err)
		}
		if items, _ := app.recoveryRecords(); listErr == nil || len(items) != 1 {
			t.Fatalf("%s: listed without error (%v), records left %+v", name, listErr, items)
		}
	}
}

// A record whose write failed is written with the next event; each turn end writes it again so the
// panel lists the latest activity first.
func TestRecoveryRecordIsWrittenAgain(t *testing.T) {
	app, _, st := newApp(t)
	p := provider.Provider{ID: "p1", Name: "测试供应商", Agent: session.ClaudeCode}
	st.SaveProvider(p)
	sbx := sandbox.Sandbox{ID: "sbx-retry", Name: "retry", Bindings: []sandbox.Binding{{Agent: session.ClaudeCode, ProviderID: p.ID}}}
	st.SaveSandbox(sbx)
	c := &conversation{id: "t-retry", sandbox: sbx, agent: session.ClaudeCode, provider: p}
	if err := os.Chmod(st.Dir(), 0o500); err != nil {
		t.Fatal(err)
	}
	failed := !app.noteNativeSession(c, "session-a")
	os.Chmod(st.Dir(), 0o700)
	app.noteNativeSession(c, "session-a")
	items, _ := app.recoveryRecords()
	if !failed || items["t-retry"].NativeSessionID != "session-a" {
		t.Fatalf("first write failed: %v; record %+v", failed, items["t-retry"])
	}
	later := items["t-retry"].UpdatedAt.Add(time.Minute)
	app.now = func() time.Time { return later }
	app.touchConversationRecovery(c)
	items, _ = app.recoveryRecords()
	if !items["t-retry"].UpdatedAt.Equal(later) {
		t.Fatalf("turn end did not move the record forward: %v", items["t-retry"].UpdatedAt)
	}
}

// Closing a tab removes its record at once, while the turn is still stopping.
func TestClosingRemovesTheRecordAtOnce(t *testing.T) {
	app, _, st := newApp(t)
	p := provider.Provider{ID: "p1", Name: "测试供应商", Agent: session.Codex}
	st.SaveProvider(p)
	sbx := sandbox.Sandbox{ID: "sbx-close", Name: "close", Bindings: []sandbox.Binding{{Agent: session.Codex, ProviderID: p.ID}}}
	st.SaveSandbox(sbx)
	turn := make(chan struct{})
	c := &conversation{id: "t-closing", sandbox: sbx, agent: session.Codex, provider: p, nativeID: "native-1", recorded: "native-1", busy: true, done: turn, stop: func() {}}
	if err := app.saveConversationRecovery(app.conversationRecoveryFrom(c)); err != nil {
		t.Fatal(err)
	}
	app.conversations.items = map[string]*conversation{c.id: c}
	closed := make(chan struct{})
	go func() { app.CloseConversation(c.id); close(closed) }()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if items, _ := app.recoveryRecords(); len(items) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the record stayed while the turn was stopping")
		}
	}
	close(turn)
	<-closed
}
