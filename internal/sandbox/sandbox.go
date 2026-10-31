// Package sandbox describes long-lived isolated environments. Each sandbox installs a set of
// agents and binds each agent to one provider; several sandboxes may share a provider.
package sandbox

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/session"
)

type Binding struct {
	Agent      session.Agent `json:"agent"`
	ProviderID string        `json:"provider_id,omitempty"`
}

type Limits struct {
	MaxRequests uint64 `json:"max_requests,omitempty"`
	MaxTokens   uint64 `json:"max_tokens,omitempty"`
}

// Usage is the cumulative quota usage carried across gatekeeper restarts.
type Usage struct {
	Requests uint64 `json:"requests"`
	Tokens   uint64 `json:"tokens"`
}

// ProviderUsage is the cumulative usage of one provider within one sandbox.
type ProviderUsage struct {
	Requests     uint64 `json:"requests"`
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
}

// UsageCursor remembers the per-provider counters last seen from the current gatekeeper run, so
// that polling adds only the difference and an app restart does not count a run twice.
type UsageCursor struct {
	RunID     string                   `json:"run_id"`
	Providers map[string]ProviderUsage `json:"providers"`
}

type Sandbox struct {
	ID                   string                   `json:"id"`
	Name                 string                   `json:"name"`
	ProjectPath          string                   `json:"project_path"`
	DemoCase             string                   `json:"demo_case,omitempty"`
	Bindings             []Binding                `json:"bindings"`
	AgentVersions        map[session.Agent]string `json:"agent_versions,omitempty"`
	AppliedAgentVersions map[session.Agent]string `json:"applied_agent_versions,omitempty"`
	// AppliedProviders 保存当前容器使用的非敏感配置；选择变更在重启后应用。
	AppliedBindings  []Binding                    `json:"applied_bindings,omitempty"`
	AppliedProviders map[string]provider.Provider `json:"applied_providers,omitempty"`
	// Placeholders maps provider ID to the placeholder visible inside this sandbox. Placeholders
	// are not secrets, but they are unique per sandbox and provider.
	Placeholders map[string]string `json:"placeholders"`
	Limits       Limits            `json:"limits"`
	// MCP lists enabled remote MCP services; FilesystemMCP enables the built-in, secret-free
	// filesystem server scoped to the project folder.
	MCP []string `json:"mcp,omitempty"`
	// MCPPermissions stores the allowed tools per sandbox and service. A present empty
	// list means that the service is enabled but all of its tools are denied.
	MCPPermissions map[string][]string `json:"mcp_permissions,omitempty"`
	// MCPGrantURLs records, for remote services, the endpoint each grant was made for. A grant
	// never applies to another address, which may be another server with tools of the same name.
	MCPGrantURLs  map[string]string `json:"mcp_grant_urls,omitempty"`
	FilesystemMCP bool              `json:"filesystem_mcp,omitempty"`
	// DevSources are the development sources (package registries, GitHub) opened for downloads.
	DevSources []string `json:"dev_sources,omitempty"`
	Usage      Usage    `json:"usage"`
	// ProviderUsage is cumulative per provider ID; UsageCursor tracks the live gatekeeper run.
	ProviderUsage map[string]ProviderUsage `json:"provider_usage,omitempty"`
	UsageCursor   *UsageCursor             `json:"usage_cursor,omitempty"`
	CreatedAt     time.Time                `json:"created_at"`
}

// AddLive folds a live per-provider snapshot from gatekeeper run runID into the cumulative counters.
func (s *Sandbox) AddLive(runID string, live map[string]ProviderUsage, quotaRequests, quotaTokens uint64) {
	if s.UsageCursor == nil || s.UsageCursor.RunID != runID {
		s.UsageCursor = &UsageCursor{RunID: runID, Providers: map[string]ProviderUsage{}}
	}
	if s.ProviderUsage == nil {
		s.ProviderUsage = map[string]ProviderUsage{}
	}
	for id, current := range live {
		previous := s.UsageCursor.Providers[id]
		total := s.ProviderUsage[id]
		total.Requests += delta(current.Requests, previous.Requests)
		total.InputTokens += delta(current.InputTokens, previous.InputTokens)
		total.OutputTokens += delta(current.OutputTokens, previous.OutputTokens)
		s.ProviderUsage[id] = total
		s.UsageCursor.Providers[id] = current
	}
	s.Usage.Requests = max(s.Usage.Requests, quotaRequests)
	s.Usage.Tokens = max(s.Usage.Tokens, quotaTokens)
}

func delta(current, previous uint64) uint64 {
	if current < previous {
		return current // counter reset without a new run ID: count what we see
	}
	return current - previous
}

// NewID returns an identifier usable in Docker object names.
func NewID() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return "sbx-" + strings.ToLower(strings.TrimRight(base32(b[:]), "="))
}

func base32(b []byte) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	var out strings.Builder
	var buffer, bits uint
	for _, value := range b {
		buffer = buffer<<8 | uint(value)
		bits += 8
		for bits >= 5 {
			out.WriteByte(alphabet[(buffer>>(bits-5))&31])
			bits -= 5
		}
	}
	return out.String()
}

// NewPlaceholder returns a fresh placeholder for one provider in one sandbox.
func NewPlaceholder() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return "cage-" + base64.RawURLEncoding.EncodeToString(b[:])
}

// Agents lists the installed agents in a stable order.
func (s Sandbox) Agents() []session.Agent {
	var result []session.Agent
	for _, agent := range []session.Agent{session.Codex, session.ClaudeCode, session.DSH} {
		for _, binding := range s.Bindings {
			if binding.Agent == agent {
				result = append(result, agent)
				break
			}
		}
	}
	return result
}

// ProviderFor returns the provider bound to agent, or "".
func (s Sandbox) ProviderFor(agent session.Agent) string {
	for _, binding := range s.Bindings {
		if binding.Agent == agent {
			return binding.ProviderID
		}
	}
	return ""
}

// Workspace is the only host directory shared with the dedicated VM.
func Workspace() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "CageWorkspace")
}

func (s Sandbox) Validate() error {
	if !strings.HasPrefix(s.ID, "sbx-") || len(s.ID) > 32 {
		return errors.New("沙箱 ID 无效")
	}
	name := strings.TrimSpace(s.Name)
	if name == "" || len([]rune(name)) > 60 {
		return errors.New("沙箱名称须为 1 至 60 个字符")
	}
	if err := ValidateProject(s.ProjectPath); err != nil {
		return err
	}
	seen := map[session.Agent]bool{}
	for _, binding := range s.Bindings {
		if !binding.Agent.Valid() {
			return fmt.Errorf("不支持的 Agent：%s", binding.Agent)
		}
		if seen[binding.Agent] {
			return fmt.Errorf("Agent 重复：%s", binding.Agent)
		}
		seen[binding.Agent] = true
	}
	return nil
}

// ValidateProject requires a real directory inside ~/CageWorkspace (after resolving symlinks),
// because that is the only directory the VM can see.
func ValidateProject(project string) error {
	if !filepath.IsAbs(project) || strings.ContainsAny(project, "\x00\n") {
		return errors.New("项目路径必须是绝对路径")
	}
	workspace, err := filepath.EvalSymlinks(Workspace())
	if err != nil {
		return errors.New("工作区 ~/CageWorkspace 不存在")
	}
	resolved, err := filepath.EvalSymlinks(project)
	if err != nil {
		return errors.New("项目目录不存在")
	}
	if resolved != project {
		return errors.New("项目路径不能包含符号链接")
	}
	rel, err := filepath.Rel(workspace, resolved)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return errors.New("项目目录必须位于 ~/CageWorkspace 之内")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return errors.New("项目路径不是目录")
	}
	return nil
}

// CheckProjectOverlap refuses a project that is the project of another sandbox in others (the
// sandbox id aside), lies inside one or contains one. A folder belongs to one sandbox: the agent of
// an outer sandbox can rename the inner project or swap it for a link, a FIFO or an app, which the
// Mac and the inner sandbox then take for the project; a shared folder also passes data between
// sandboxes that have different networks, tools and keys.
func CheckProjectOverlap(id, project string, others []Sandbox) error {
	other, found := OverlappingSandbox(id, project, others)
	switch {
	case !found:
		return nil
	case within(project, other.ProjectPath) && within(other.ProjectPath, project):
		return fmt.Errorf("这个文件夹已是沙箱「%s」的项目，一个文件夹只能给一个沙箱用，请换一个文件夹", other.Name)
	case within(other.ProjectPath, project):
		return fmt.Errorf("这个文件夹在沙箱「%s」的项目里面，请换一个与它并列的文件夹", other.Name)
	}
	return fmt.Errorf("这个文件夹里面有沙箱「%s」的项目，请换一个不包含它的文件夹", other.Name)
}

// OverlappingSandbox returns the first sandbox in others, the sandbox id aside, whose project is
// the same folder as project, lies inside it or contains it.
func OverlappingSandbox(id, project string, others []Sandbox) (Sandbox, bool) {
	for _, other := range others {
		if other.ID != id && (within(other.ProjectPath, project) || within(project, other.ProjectPath)) {
			return other, true
		}
	}
	return Sandbox{}, false
}

// within reports whether inner is the folder outer or lies inside it. Paths compare by component
// and ignore case, as the Mac's file system does by default, so "proj-2" is not inside "proj" and a
// trailing slash changes nothing. Folders that exist also compare by identity, which catches other
// spellings of the same folder, such as another Unicode form of its name.
func within(outer, inner string) bool {
	if !filepath.IsAbs(outer) || !filepath.IsAbs(inner) {
		return false
	}
	outerParts := strings.Split(filepath.Clean(outer), string(filepath.Separator))
	innerParts := strings.Split(filepath.Clean(inner), string(filepath.Separator))
	if len(innerParts) >= len(outerParts) && slices.EqualFunc(outerParts, innerParts[:len(outerParts)], strings.EqualFold) {
		return true
	}
	target, err := os.Stat(outer)
	if err != nil {
		return false
	}
	for dir := filepath.Clean(inner); ; dir = filepath.Dir(dir) {
		if info, err := os.Stat(dir); err == nil && os.SameFile(info, target) {
			return true
		}
		if dir == filepath.Dir(dir) {
			return false
		}
	}
}
