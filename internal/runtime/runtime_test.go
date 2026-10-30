package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
	"github.com/shaoxier/AIofCage/internal/store"
)

func TestAgentFilesContainOnlyPlaceholders(t *testing.T) {
	codex := provider.Provider{ID: "xapi", Name: `七牛 "xapi"`, Agent: session.Codex, Protocol: provider.OpenAIResponses,
		BaseURL: "https://xapi.qiniu.io/v1", Model: "gpt-6.1-sol", Auth: provider.AuthBearer, KeyRef: "k"}
	claude := provider.Provider{ID: "ds", Name: "DeepSeek", Agent: session.ClaudeCode, Protocol: provider.Anthropic,
		BaseURL: "https://api.deepseek.com/anthropic", Model: "deepseek-chat", Auth: provider.AuthXAPIKey, KeyRef: "k2"}
	sbx := sandbox.Sandbox{ID: "sbx-x", Name: "it's mine", ProjectPath: "/Users/u/CageWorkspace/p",
		Bindings:     []sandbox.Binding{{Agent: session.Codex, ProviderID: "xapi"}, {Agent: session.ClaudeCode, ProviderID: "ds"}, {Agent: session.DSH}},
		Placeholders: map[string]string{"xapi": "cage-AAAAAAAAAAAAAAAAAAAA", "ds": "cage-BBBBBBBBBBBBBBBBBBBB"}}
	files := agentFiles(sbx, map[string]provider.Provider{"xapi": codex, "ds": claude}, nil)
	byPath := map[string]string{}
	for _, f := range files {
		byPath[f.Path] = f.Content
		if f.Mode != 0o600 {
			t.Errorf("%s mode %o", f.Path, f.Mode)
		}
	}
	env := byPath[".aiofcage/env"]
	for _, want := range []string{"export AIOFCAGE_CODEX_API_KEY='cage-AAAAAAAAAAAAAAAAAAAA'",
		"export ANTHROPIC_API_KEY='cage-BBBBBBBBBBBBBBBBBBBB'", "export ANTHROPIC_BASE_URL='https://api.deepseek.com/anthropic'",
		`export AIOFCAGE_SANDBOX='it'"'"'s mine'`} {
		if !strings.Contains(env, want) {
			t.Errorf("env missing %q:\n%s", want, env)
		}
	}
	config := byPath[".codex/config.toml"]
	for _, want := range []string{`model = "gpt-6.1-sol"`, `name = "七牛 \"xapi\""`, `wire_api = "responses"`,
		`env_key = "AIOFCAGE_CODEX_API_KEY"`, `[projects."/Users/u/CageWorkspace/p"]`} {
		if !strings.Contains(config, want) {
			t.Errorf("codex config missing %q:\n%s", want, config)
		}
	}
	if strings.Contains(config, "cage-") {
		t.Error("codex config must reference the placeholder through env_key only")
	}
}

func TestConnectorImageContext(t *testing.T) {
	build, err := connectorContext()
	if err != nil {
		t.Skip("connector binary not built (make connector-linux)")
	}
	dockerfile := string(build.files["Dockerfile"])
	if strings.Contains(dockerfile, "server-postgres") || !strings.Contains(dockerfile, "npm ci") || !strings.Contains(dockerfile, "--ignore-scripts") ||
		!strings.Contains(dockerfile, "USER 1000:1000") || !strings.HasPrefix(build.tag, "aiofcage/connector:") {
		t.Fatalf("connector image:\n%s", dockerfile)
	}
	if len(build.files["postgres-policy.mjs"]) == 0 || len(build.files["postgres-server.mjs"]) == 0 || len(build.files["postgres-package-lock.json"]) == 0 {
		t.Fatal("连接器未打包只读策略、服务或依赖锁")
	}
}

func TestAgentImageContext(t *testing.T) {
	a, err := agentContext([]session.Agent{session.DSH, session.Codex})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := agentContext([]session.Agent{session.Codex, session.DSH})
	if a.tag != b.tag || !strings.HasPrefix(a.tag, "aiofcage/agent:") {
		t.Fatalf("tag must not depend on selection order: %s %s", a.tag, b.tag)
	}
	dockerfile := string(a.files["Dockerfile"])
	codex := strings.Index(dockerfile, "@openai/codex@0.164.0")
	dsh := strings.Index(dockerfile, "@deepseek-ai/dsh@")
	if codex < 0 || dsh < 0 || codex > dsh || strings.Contains(dockerfile, "claude-code") {
		t.Fatalf("unexpected Dockerfile:\n%s", dockerfile)
	}
	if !strings.Contains(dockerfile, "public.ecr.aws/docker/library/node:24.22.1-bookworm-slim@sha256:") {
		t.Fatal("base image must be pinned from public.ecr.aws")
	}
	empty, err := agentContext(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range Catalog() {
		if strings.Contains(string(empty.files["Dockerfile"]), spec.Package+"@") {
			t.Fatalf("空白镜像包含 %s", spec.Name)
		}
	}
	if !strings.Contains(string(empty.files["Dockerfile"]), "USER 1000:1000") {
		t.Fatal("空白镜像缺少非特权用户")
	}
}

func TestParseSize(t *testing.T) {
	for value, want := range map[string]uint64{"512B": 512, "1.5KiB": 1536, "12MiB": 12 << 20, "2kB": 2000, " 1.2GB ": 1_200_000_000, "bogus": 0} {
		if got := parseSize(value); got != want {
			t.Errorf("%q: got %d want %d", value, got, want)
		}
	}
}

func TestMCPConfigUsesPlaceholdersOnly(t *testing.T) {
	codex := provider.Provider{ID: "xapi", Name: "x", Agent: session.Codex, Protocol: provider.OpenAIResponses,
		BaseURL: "https://xapi.qiniu.io/v1", Model: "m", Auth: provider.AuthBearer, KeyRef: "k"}
	github := mcp.Service{ID: "github", Name: "GitHub", URL: "https://api.githubcopilot.com/mcp/", KeyRef: mcp.KeyRefFor("github")}
	shop := mcp.Service{ID: "shop", Name: "Shop", Kind: mcp.KindPostgres, URL: "postgresql://app@localhost/shop", KeyRef: mcp.KeyRefFor("shop")}
	sbx := sandbox.Sandbox{ID: "sbx-x", Name: "n", ProjectPath: "/Users/u/CageWorkspace/p", FilesystemMCP: true, MCP: []string{"github", "shop"},
		Bindings: []sandbox.Binding{{Agent: session.Codex, ProviderID: "xapi"}, {Agent: session.ClaudeCode}},
		Placeholders: map[string]string{"xapi": "cage-AAAAAAAAAAAAAAAAAAAA", "mcp:github": "cage-GGGGGGGGGGGGGGGGGGGG",
			"mcp:shop": "cage-SSSSSSSSSSSSSSSSSSSS"}}
	files := agentFiles(sbx, map[string]provider.Provider{"xapi": codex}, map[string]mcp.Service{"github": github, "shop": shop})
	byPath := map[string]agentFile{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	config := byPath[".codex/config.toml"].Content
	if strings.Contains(config, "api.githubcopilot.com") {
		t.Errorf("agents must only know the gateway address of a remote service:\n%s", config)
	}
	for _, want := range []string{"[mcp_servers.github]", `url = "http://gatekeeper:8890/mcp/github"`,
		`bearer_token_env_var = "AIOFCAGE_MCP_GITHUB_TOKEN"`, "[mcp_servers.filesystem]", `args = ["/Users/u/CageWorkspace/p"]`,
		"[mcp_servers.shop]", `url = "http://gatekeeper:8890/mcp/shop"`, `bearer_token_env_var = "AIOFCAGE_MCP_SHOP_TOKEN"`} {
		if !strings.Contains(config, want) {
			t.Errorf("codex config missing %q:\n%s", want, config)
		}
	}
	if !strings.Contains(byPath[".aiofcage/env"].Content, "export AIOFCAGE_MCP_GITHUB_TOKEN='cage-GGGGGGGGGGGGGGGGGGGG'") {
		t.Error("env missing MCP placeholder")
	}
	claude := byPath[".claude.json"]
	if !claude.MergeJSON || len(claude.ReplaceKeys) != 1 || !strings.Contains(claude.Content, `"Authorization":"Bearer cage-GGGGGGGGGGGGGGGGGGGG"`) ||
		!strings.Contains(claude.Content, `"command":"mcp-server-filesystem"`) ||
		!strings.Contains(claude.Content, `"url":"http://gatekeeper:8890/mcp/shop"`) || strings.Contains(claude.Content, "postgresql://") {
		t.Errorf("claude state: %+v", claude)
	}
}

// SetMCP once deadlocked: it read the store inside the store's own update callback.
func TestSetMCPUpdatesStoppedSandbox(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{store: st, docker: runner{path: "/usr/bin/false"}, gates: map[string]*gateClient{}, locks: map[string]*sync.Mutex{}}
	github := mcp.Service{ID: "github", Name: "GitHub", URL: "https://api.githubcopilot.com/mcp/", KeyRef: mcp.KeyRefFor("github")}
	if err := st.SaveMCPService(github); err != nil {
		t.Fatal(err)
	}
	sbx := sandbox.Sandbox{ID: "sbx-test0001", Name: "t", ProjectPath: "/Users/u/CageWorkspace/p",
		Bindings: []sandbox.Binding{{Agent: session.Codex}}, Placeholders: map[string]string{}}
	if err := st.SaveSandbox(sbx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.SetMCP(context.Background(), sbx.ID, []string{"github"}, true) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SetMCP deadlocked")
	}
	saved, _ := st.Sandbox(sbx.ID)
	if len(saved.MCP) != 1 || !saved.FilesystemMCP || !strings.HasPrefix(saved.Placeholders["mcp:github"], "cage-") {
		t.Fatalf("%+v", saved)
	}
	postgres := mcp.Service{ID: "shop", Name: "Shop", Kind: mcp.KindPostgres, URL: "postgresql://app@localhost/shop", KeyRef: mcp.KeyRefFor("shop")}
	if err := st.SaveMCPService(postgres); err != nil {
		t.Fatal(err)
	}
	if err := m.SetMCPPermissions(context.Background(), sbx.ID, []string{"shop"}, false, map[string][]string{"shop": {}}); err != nil {
		t.Fatal(err)
	}
	saved, _ = st.Sandbox(sbx.ID)
	if len(saved.MCP) != 1 || saved.MCP[0] != "shop" || saved.FilesystemMCP || saved.MCPPermissions == nil {
		t.Fatalf("MCP permissions were not persisted: %+v", saved)
	}
	if tools, ok := saved.MCPPermissions["shop"]; !ok || len(tools) != 0 {
		t.Fatalf("explicit empty tool grant was not preserved: %#v", saved.MCPPermissions)
	}
	if err := m.SetMCPPermissions(context.Background(), sbx.ID, []string{"shop"}, false, map[string][]string{"shop": {"drop_table"}}); err == nil {
		t.Fatal("unknown connector tools must be rejected")
	}
	if err := m.SetMCPPermissions(context.Background(), sbx.ID, []string{"github"}, false, map[string][]string{"shop": {}}); err == nil {
		t.Fatal("permissions for disabled services must be rejected")
	}
	if err := m.SetMCP(context.Background(), sbx.ID, []string{"missing"}, false); err == nil {
		t.Fatal("unknown services must be rejected")
	}
}

func TestRevokeRuntimeClearsConnectorTokens(t *testing.T) {
	m := &Manager{gates: map[string]*gateClient{}, connectorTokens: map[string]string{
		"aiofcage-sbx-test0001-mcp-shop": "connector-secret",
		"aiofcage-sbx-other-mcp-shop":    "keep",
	}}
	m.revokeRuntimeTokens("sbx-test0001")
	if _, ok := m.connectorTokens["aiofcage-sbx-test0001-mcp-shop"]; ok {
		t.Fatal("sandbox connector token survived revocation")
	}
	if got := m.connectorTokens["aiofcage-sbx-other-mcp-shop"]; got != "keep" {
		t.Fatalf("revocation touched another sandbox token: %q", got)
	}
}

func TestDSHCustomProviderConfig(t *testing.T) {
	p := provider.Provider{ID: "custom", Agent: session.DSH, Protocol: provider.OpenAIResponses, BaseURL: "https://gateway.example/v1", Model: "custom-model"}
	sbx := sandbox.Sandbox{Bindings: []sandbox.Binding{{Agent: session.DSH, ProviderID: p.ID}}, Placeholders: map[string]string{p.ID: "cage-placeholder"}}
	files := agentFiles(sbx, map[string]provider.Provider{p.ID: p}, nil)
	var patch, env string
	for _, f := range files {
		if f.Path == ".dsh/cordis.patch.yml" {
			patch = f.Content
		}
		if f.Path == ".aiofcage/env" {
			env = f.Content
		}
	}
	for _, want := range []string{"openai-responses", "https://gateway.example/v1", "custom-model", "agent-default-model", "apiKeyEnv: AIOFCAGE_DSH_API_KEY"} {
		if !strings.Contains(patch, want) {
			t.Fatalf("缺少 %s", want)
		}
	}
	if !strings.Contains(env, "cage-placeholder") || strings.Contains(patch, "cage-placeholder") {
		t.Fatal("凭据引用不正确")
	}
	if !strings.Contains(patch, "input: [text]") || strings.Contains(patch, "input: [text, image]") {
		t.Fatal("未开启图片能力时不得宣称支持图片")
	}
	p.ImageInput = true
	if !strings.Contains(dshConfig(p), "input: [text, image]") {
		t.Fatal("图片能力没有传递给 DSH 模型路由")
	}
}

func TestResetKeepsSandboxConfigurationAndHonorsHomeChoice(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "docker.log")
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$AIOFCAGE_TEST_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIOFCAGE_TEST_LOG", logPath)
	st, err := store.Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	sbx := sandbox.Sandbox{ID: "sbx-reset0001", Name: "reset", ProjectPath: "/Users/u/CageWorkspace/reset",
		Bindings:             []sandbox.Binding{{Agent: session.Codex, ProviderID: "provider"}},
		AppliedBindings:      []sandbox.Binding{{Agent: session.Codex, ProviderID: "provider"}},
		AppliedProviders:     map[string]provider.Provider{"provider": {ID: "provider", Name: "Provider"}},
		AppliedAgentVersions: map[session.Agent]string{session.Codex: "old"},
		Placeholders:         map[string]string{"provider": "cage-placeholder"}}
	if err := st.SaveSandbox(sbx); err != nil {
		t.Fatal(err)
	}
	m := &Manager{store: st, docker: runner{path: docker}, gates: map[string]*gateClient{}, locks: map[string]*sync.Mutex{}, connectorTokens: map[string]string{
		"aiofcage-sbx-reset0001-mcp-shop": "secret",
	}}
	if err := m.Reset(context.Background(), sbx.ID, true, false); err != nil {
		t.Fatal(err)
	}
	saved, err := st.Sandbox(sbx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ProjectPath != sbx.ProjectPath || saved.Name != sbx.Name || len(saved.Bindings) != 1 || saved.AppliedBindings != nil || saved.AppliedProviders != nil || saved.AppliedAgentVersions != nil {
		t.Fatalf("reset changed stored configuration: %+v", saved)
	}
	if _, ok := m.connectorTokens["aiofcage-sbx-reset0001-mcp-shop"]; ok {
		t.Fatal("reset left connector token in memory")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "volume rm") {
		t.Fatal("keepHome reset removed the Agent home volume")
	}
}
