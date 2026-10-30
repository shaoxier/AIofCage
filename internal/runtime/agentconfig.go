package runtime

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/shaoxier/AIofCage/internal/devsource"
	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
)

// agentFile is written into /home/agent by aiofcage-init.py. Contents are never secret: they hold
// placeholders, base URLs and model names only.
type agentFile struct {
	Path    string `json:"path"`
	Mode    int    `json:"mode"`
	Content string `json:"content"`
	// MergeJSON merges Content into an existing JSON file instead of replacing it; ReplaceKeys
	// are top-level keys AIofCage owns entirely and replaces rather than merges.
	MergeJSON   bool     `json:"merge_json,omitempty"`
	ReplaceKeys []string `json:"replace_keys,omitempty"`
}

// tomlString renders a TOML basic string. JSON string escaping is a subset of TOML's.
func tomlString(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// agentFiles renders configuration for every installed agent. An agent without a provider gets
// no model configuration and will report that it is not configured.
func agentFiles(sbx sandbox.Sandbox, providers map[string]provider.Provider, services map[string]mcp.Service) []agentFile {
	env := map[string]string{"AIOFCAGE_SANDBOX": sbx.Name}
	for _, setting := range devsource.Env(sbx.DevSources) {
		env[setting[0]] = setting[1]
	}
	for _, id := range sbx.MCP {
		if _, ok := services[id]; ok {
			env[mcp.EnvVar(id)] = sbx.Placeholders[mcp.PlaceholderKey(id)]
		}
	}
	var files []agentFile
	for _, agent := range sbx.Agents() {
		p, hasProvider := providers[sbx.ProviderFor(agent)]
		if agent == session.ClaudeCode {
			// Written even without a provider: onboarding, project trust and MCP servers.
			files = append(files, agentFile{Path: ".claude.json", Mode: 0o600, MergeJSON: true,
				ReplaceKeys: []string{"mcpServers"}, Content: claudeState(sbx, p, hasProvider, services)})
		}
		if !hasProvider {
			continue
		}
		placeholder := sbx.Placeholders[p.ID]
		switch agent {
		case session.Codex:
			env["AIOFCAGE_CODEX_API_KEY"] = placeholder
			files = append(files, agentFile{Path: ".codex/config.toml", Mode: 0o600, Content: codexConfig(sbx, p, services)})
		case session.ClaudeCode:
			env["ANTHROPIC_BASE_URL"] = p.BaseURL
			if p.Auth == provider.AuthXAPIKey {
				env["ANTHROPIC_API_KEY"] = placeholder
			} else {
				env["ANTHROPIC_AUTH_TOKEN"] = placeholder
			}
			env["ANTHROPIC_MODEL"] = p.Model
			env["ANTHROPIC_SMALL_FAST_MODEL"] = p.Model
			env["DISABLE_TELEMETRY"] = "1"
			env["DISABLE_ERROR_REPORTING"] = "1"
			env["DISABLE_AUTOUPDATER"] = "1"
			env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
		case session.DSH:
			console, _ := assets.ReadFile("assets/agent/dsh-console.py")
			files = append(files, agentFile{Path: ".aiofcage/dsh-console.py", Mode: 0o600, Content: string(console)})
			env["AIOFCAGE_DSH_API_KEY"] = placeholder
			env["DSH_HOME"] = "/home/agent/.dsh"
			files = append(files, agentFile{Path: ".dsh/cordis.patch.yml", Mode: 0o600, Content: dshConfig(p) + dshMCPConfig(sbx, services)})
			env["DSH_TELEMETRY_DISABLED"] = "1"
		}
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var script strings.Builder
	script.WriteString("# 由 AIofCage 生成，切换供应商时会被覆盖。这里只有占位符，没有真实密钥。\n")
	for _, key := range keys {
		fmt.Fprintf(&script, "export %s=%s\n", key, shellQuote(env[key]))
	}
	return append(files, agentFile{Path: ".aiofcage/env", Mode: 0o600, Content: script.String()})
}

func codexConfig(sbx sandbox.Sandbox, p provider.Provider, services map[string]mcp.Service) string {
	var b strings.Builder
	b.WriteString("# 由 AIofCage 生成，切换供应商时会被覆盖。\n")
	b.WriteString("# 沙箱本身就是隔离边界，所以 Codex 内部不再叠加自己的沙箱。\n")
	fmt.Fprintf(&b, "model = %s\n", tomlString(p.Model))
	b.WriteString("model_provider = \"aiofcage\"\n")
	b.WriteString("approval_policy = \"on-request\"\n")
	b.WriteString("sandbox_mode = \"danger-full-access\"\n\n")
	b.WriteString("[model_providers.aiofcage]\n")
	fmt.Fprintf(&b, "name = %s\n", tomlString(p.Name))
	fmt.Fprintf(&b, "base_url = %s\n", tomlString(p.BaseURL))
	b.WriteString("env_key = \"AIOFCAGE_CODEX_API_KEY\"\n")
	b.WriteString("wire_api = \"responses\"\n\n")
	fmt.Fprintf(&b, "[projects.%s]\n", tomlString(sbx.ProjectPath))
	b.WriteString("trust_level = \"trusted\"\n")
	for _, id := range sbx.MCP {
		service, ok := services[id]
		if !ok {
			continue
		}
		// The token comes from the environment and is only a placeholder. HTTP services get the real
		// token injected by the gatekeeper; connector services are reached through its MCP entrypoint.
		fmt.Fprintf(&b, "\n[mcp_servers.%s]\nurl = %s\nbearer_token_env_var = %s\n", id, tomlString(service.AgentURL()), tomlString(mcp.EnvVar(id)))
	}
	if sbx.FilesystemMCP {
		fmt.Fprintf(&b, "\n[mcp_servers.filesystem]\ncommand = \"mcp-server-filesystem\"\nargs = [%s]\n", tomlString(sbx.ProjectPath))
	}
	return b.String()
}

// claudeState pre-answers Claude Code's first-run questions: onboarding, trusting the project
// folder, and (for x-api-key providers) approving the API key, which it remembers by its last
// 20 characters. Only these keys are merged; the rest of ~/.claude.json is left alone.
func claudeState(sbx sandbox.Sandbox, p provider.Provider, hasProvider bool, services map[string]mcp.Service) string {
	servers := map[string]any{}
	for _, id := range sbx.MCP {
		if service, ok := services[id]; ok {
			servers[id] = map[string]any{"type": "http", "url": service.AgentURL(),
				"headers": map[string]string{"Authorization": "Bearer " + sbx.Placeholders[mcp.PlaceholderKey(id)]}}
		}
	}
	if sbx.FilesystemMCP {
		servers["filesystem"] = map[string]any{"type": "stdio", "command": "mcp-server-filesystem", "args": []string{sbx.ProjectPath}}
	}
	state := map[string]any{
		"hasCompletedOnboarding": true,
		"projects": map[string]any{
			sbx.ProjectPath: map[string]any{"hasTrustDialogAccepted": true, "hasCompletedProjectOnboarding": true},
		},
		"mcpServers": servers,
	}
	if placeholder := sbx.Placeholders[p.ID]; hasProvider && p.Auth == provider.AuthXAPIKey && len(placeholder) >= 20 {
		state["customApiKeyResponses"] = map[string]any{"approved": []string{placeholder[len(placeholder)-20:]}, "rejected": []string{}}
	}
	data, _ := json.Marshal(state)
	return string(data)
}

// dshConfig 对应 npm 0.2.1-rc.2 的 llm-pi-ai 与 agent-default-model 配置。
func dshConfig(p provider.Provider) string {
	return dshConfigWithEffort(p, "")
}

func dshConfigWithEffort(p provider.Provider, effort string) string {
	api := map[provider.Protocol]string{provider.OpenAIChat: "openai-completions", provider.OpenAIResponses: "openai-responses", provider.Anthropic: "anthropic-messages"}[p.Protocol]
	input := "text"
	if p.ImageInput {
		input += ", image"
	}
	reasoning, selection := "", ""
	if effort != "" {
		reasoning = fmt.Sprintf("            reasoningEfforts:\n              %s: %s\n", tomlString(effort), tomlString(effort))
		selection = fmt.Sprintf("    reasoningEffort: %s\n", tomlString(effort))
	}
	return fmt.Sprintf("- id: llm-pi-ai\n  config:\n    providers:\n      aiofcage:\n        apiKeyEnv: AIOFCAGE_DSH_API_KEY\n        api: %s\n        baseURL: %s\n        models:\n          - id: %s\n            input: [%s]\n%s- id: agent-default-model\n  config:\n    provider: aiofcage\n    model: %s\n%s", api, tomlString(p.BaseURL), tomlString(p.Model), input, reasoning, tomlString(p.Model), selection)
}

// 官方 MCP 插件使用显式占位符标头；真正的服务凭据仍由门卫或可信连接器持有。
func dshMCPConfig(sbx sandbox.Sandbox, services map[string]mcp.Service) string {
	var b strings.Builder
	for _, id := range sbx.MCP {
		service, ok := services[id]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "- insert:\n    - id: %s\n      name: '@deepseek-ai/dsh-mcp-client'\n      config:\n        serverName: %s\n        transport: streamable-http\n        url: %s\n        headers:\n          Authorization: %s\n        failOnStartupError: true\n", tomlString("aiofcage-mcp-"+id), tomlString(id), tomlString(service.AgentURL()), tomlString("Bearer "+sbx.Placeholders[mcp.PlaceholderKey(id)]))
	}
	if sbx.FilesystemMCP {
		fmt.Fprintf(&b, "- insert:\n    - id: aiofcage-mcp-filesystem\n      name: '@deepseek-ai/dsh-mcp-client'\n      config:\n        serverName: filesystem\n        transport: stdio\n        command: mcp-server-filesystem\n        args: [%s]\n        failOnStartupError: true\n", tomlString(sbx.ProjectPath))
	}
	return b.String()
}
