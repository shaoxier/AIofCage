// Package service exposes desktop use cases to the Wails frontend. No method returns a secret:
// API keys only flow inward (to the Keychain) and from the Keychain to the gatekeeper.
package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/shaoxier/AIofCage/internal/buildinfo"
	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/oauth"
	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
	"github.com/shaoxier/AIofCage/internal/store"
	"github.com/shaoxier/AIofCage/internal/vault"
)

type DirectoryPicker func() (string, error)

type App struct {
	store      *store.Store
	vault      vault.Store
	manager    *runtime.Manager
	managerErr error
	picker     DirectoryPicker
	client     *http.Client
	// explainClient has no overall timeout; each commentary request is bounded by explainTimeout.
	explainClient *http.Client
	oauth         *oauth.Client
	now           func() time.Time
	emit          Emitter
	terminal      TerminalOpener
	monitor       *monitor
	explain       explainCache
	ops           operations
	terms         terminals
	conversations conversationSessions
	usageMu       sync.Mutex
	demos         demoLog
	securityMu    sync.Mutex
	isolationRuns map[string]SecurityIsolationRun
	prices        pricingCatalog
	updatesMu     sync.Mutex
	onboardingMu  sync.Mutex
}

// noRedirect keeps credentials on the host they were sent to. Go would carry custom headers such
// as X-Api-Key to another site on a redirect, and it does not refuse an https to http downgrade.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// NewApp wires the use cases. manager may be nil when Docker or Colima is missing; managerErr then
// explains why, and environment checks surface it to the user.
func NewApp(st *store.Store, secrets vault.Store, manager *runtime.Manager, managerErr error, picker DirectoryPicker) *App {
	// Connection tests and OAuth go straight to the service; shell proxy settings are ignored on purpose.
	client := &http.Client{Timeout: 12 * time.Second, Transport: &http.Transport{Proxy: nil, TLSHandshakeTimeout: 8 * time.Second},
		CheckRedirect: noRedirect}
	a := &App{store: st, vault: secrets, manager: manager, managerErr: managerErr, picker: picker, now: time.Now,
		emit: func(string, any) {}, terminal: openInTerminal, monitor: newMonitor(), explain: explainCache{items: map[string]Explanation{}},
		client: client, oauth: oauth.NewClient(client, openURL), isolationRuns: map[string]SecurityIsolationRun{},
		explainClient: &http.Client{Transport: &http.Transport{Proxy: nil, TLSHandshakeTimeout: 10 * time.Second}, CheckRedirect: noRedirect}}
	if manager != nil {
		manager.BeforeReplace = a.closeSandboxConversations
		// Progress is remembered as well as emitted, so a page opened later still shows it.
		manager.Progress = func(id, message string) {
			a.ops.setProgress(id, message)
			a.emit("sandbox:progress", map[string]string{"id": id, "message": message})
		}
	}
	return a
}

type AgentInfo struct {
	ID      session.Agent `json:"id"`
	Name    string        `json:"name"`
	Version string        `json:"version"`
	Command string        `json:"command"`
}

type Overview struct {
	Version     string         `json:"version"`
	Stage       string         `json:"stage"`
	Environment runtime.Report `json:"environment"`
	Agents      []AgentInfo    `json:"agents"`
	DataDir     string         `json:"data_dir"`
	Workspace   string         `json:"workspace"`
}

func (a *App) GetOverview() Overview {
	overview := Overview{Version: buildinfo.Version, Stage: buildinfo.Stage, DataDir: a.store.Dir(), Workspace: sandbox.Workspace()}
	for _, spec := range runtime.Catalog() {
		overview.Agents = append(overview.Agents, AgentInfo{spec.Agent, spec.Name, spec.Version, spec.Command})
	}
	if a.manager == nil {
		detail := "缺少运行环境"
		if a.managerErr != nil {
			detail = a.managerErr.Error()
		}
		overview.Environment = runtime.Report{Checks: []runtime.Check{{ID: "tools", Name: "Docker / Colima", Status: "missing", Detail: detail}}}
		return overview
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	overview.Environment = a.manager.Environment().Inspect(ctx)
	return overview
}

func (a *App) SelectProject() (string, error) {
	if a.picker == nil {
		return "", errors.New("项目选择器不可用，请在桌面应用中操作")
	}
	return a.picker()
}

// ProviderView never contains the key itself, only whether one is saved.
type ProviderView struct {
	provider.Provider
	KeySaved   bool     `json:"key_saved"`
	KeyManaged bool     `json:"key_managed"`
	UsedBy     []string `json:"used_by"`
}

func (a *App) ListPresets() []provider.Preset { return provider.Presets() }

func (a *App) ListProviders() ([]ProviderView, error) {
	providers, err := a.store.Providers()
	if err != nil {
		return nil, err
	}
	sandboxes, err := a.store.Sandboxes()
	if err != nil {
		return nil, err
	}
	views := make([]ProviderView, 0, len(providers))
	for _, p := range providers {
		views = append(views, a.view(p, sandboxes))
	}
	return views, nil
}

func (a *App) view(p provider.Provider, sandboxes []sandbox.Sandbox) ProviderView {
	view := ProviderView{Provider: p, KeyManaged: p.KeyRef == provider.KeyRefFor(p.ID), UsedBy: []string{}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.vault.Read(ctx, p.KeyRef); err == nil {
		view.KeySaved = true
	}
	for _, s := range sandboxes {
		bindings := append(append([]sandbox.Binding{}, s.Bindings...), s.AppliedBindings...)
		used := false
		for _, binding := range bindings {
			if binding.ProviderID == p.ID {
				used = true
			}
		}
		if used {
			view.UsedBy = append(view.UsedBy, s.Name)
		}
	}
	return view
}

type SaveProviderRequest struct {
	Pricing    *provider.Pricing `json:"pricing"`
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Agent      session.Agent     `json:"agent"`
	Protocol   provider.Protocol `json:"protocol"`
	BaseURL    string            `json:"base_url"`
	Model      string            `json:"model"`
	ImageInput bool              `json:"image_input"`
	Auth       provider.Auth     `json:"auth"`
	Preset     string            `json:"preset"`
	// APIKey is write-only: it goes straight to the Keychain and is never returned.
	APIKey string `json:"api_key"`
	// ExistingKeyRef reuses a Keychain item. Only AIofCage's own items and items named by a
	// preset are allowed, so the UI cannot be used to point a provider at arbitrary secrets.
	ExistingKeyRef string `json:"existing_key_ref"`
}

func (a *App) SaveProvider(request SaveProviderRequest) (ProviderView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var p provider.Provider
	existing := request.ID != ""
	if existing {
		current, err := a.store.Provider(request.ID)
		if err != nil {
			return ProviderView{}, err
		}
		p = current
	} else {
		p = provider.Provider{ID: newProviderID(), CreatedAt: a.now().UTC()}
	}
	sandboxes, err := a.store.Sandboxes()
	if err != nil {
		return ProviderView{}, err
	}
	used := a.view(p, sandboxes).UsedBy
	if existing && request.Agent != p.Agent && len(used) > 0 {
		return ProviderView{}, fmt.Errorf("还有沙箱在用这个供应商（%s），不能改成别的 Agent", strings.Join(used, "、"))
	}
	if existing && a.manager != nil {
		for _, s := range sandboxes {
			if s.ProviderFor(p.Agent) == p.ID {
				if err := a.manager.CaptureApplied(ctx, s.ID); err != nil {
					return ProviderView{}, err
				}
			}
		}
	}
	p.Name, p.Agent, p.Protocol, p.BaseURL, p.Model, p.Auth, p.Preset =
		request.Name, request.Agent, request.Protocol, request.BaseURL, request.Model, request.Auth, request.Preset
	p.ImageInput = request.Agent == session.DSH && request.ImageInput
	if request.Pricing != nil {
		price := *request.Pricing
		if p.Pricing != nil {
			price.Version = p.Pricing.Version
		}
		if p.Pricing == nil || price != *p.Pricing {
			price.Version = a.now().UTC().Format(time.RFC3339Nano)
		}
		p.Pricing = &price
	} else {
		p.Pricing = nil
	}
	key := strings.TrimSpace(request.APIKey)
	switch {
	case request.ExistingKeyRef != "":
		if !allowedExistingKeyRef(request.ExistingKeyRef, p.ID) {
			return ProviderView{}, errors.New("只能使用 AIofCage 保存的密钥，或预设中列出的钥匙串条目")
		}
		p.KeyRef = request.ExistingKeyRef
	case key != "" || !existing:
		p.KeyRef = provider.KeyRefFor(p.ID)
	}
	p.Normalize()
	if err := p.Validate(); err != nil {
		return ProviderView{}, err
	}
	switch {
	case key != "":
		// 已生效快照仍引用同一钥匙串条目时，覆盖会改变后续策略刷新使用的密钥。
		// 先停止这些沙箱，再更新条目，确保“重启后生效”的行为不会被绕过。
		currentSandboxes, err := a.store.Sandboxes()
		if err != nil {
			return ProviderView{}, err
		}
		for _, s := range currentSandboxes {
			for _, applied := range s.AppliedProviders {
				if applied.KeyRef == p.KeyRef {
					return ProviderView{}, fmt.Errorf("请先停止沙箱「%s」，再更新它正在使用的 API Key", s.Name)
				}
			}
		}
		if err := a.vault.Put(ctx, p.KeyRef, []byte(key)); err != nil {
			return ProviderView{}, err
		}
	case request.ExistingKeyRef != "":
		if _, err := a.vault.Read(ctx, p.KeyRef); err != nil {
			return ProviderView{}, fmt.Errorf("钥匙串中找不到条目 %s", p.KeyRef)
		}
	case !existing:
		return ProviderView{}, errors.New("请填写 API Key")
	}
	if err := a.store.SaveProvider(p); err != nil {
		return ProviderView{}, err
	}

	return a.view(p, sandboxes), nil
}

func allowedExistingKeyRef(ref, providerID string) bool {
	if ref == provider.KeyRefFor(providerID) {
		return true
	}
	for _, preset := range provider.Presets() {
		if preset.ExistingKeyRef != "" && preset.ExistingKeyRef == ref {
			return true
		}
	}
	return false
}

func newProviderID() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	var b [6]byte
	_, _ = rand.Read(b[:])
	id := []byte("p-")
	for _, value := range b {
		id = append(id, alphabet[int(value)%len(alphabet)])
	}
	return string(id)
}

func (a *App) DeleteProvider(id string) error {
	p, err := a.store.Provider(id)
	if err != nil {
		return err
	}
	sandboxes, err := a.store.Sandboxes()
	if err != nil {
		return err
	}
	if used := a.view(p, sandboxes).UsedBy; len(used) > 0 {
		return fmt.Errorf("还有沙箱在用这个供应商（%s），请先给它们换一个供应商", strings.Join(used, "、"))
	}
	if err := a.store.DeleteProvider(id); err != nil {
		return err
	}
	a.forgetRetiredUsage(p.ID)
	if p.KeyRef == provider.KeyRefFor(p.ID) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return a.vault.Delete(ctx, p.KeyRef)
	}
	return nil
}

type TestResult struct {
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Message   string `json:"message"`
}

// TestProvider checks that the endpoint is reachable and the key is accepted by listing models.
// The request is made by the trusted desktop process, never from inside a sandbox.
func (a *App) TestProvider(id string) (TestResult, error) {
	p, err := a.store.Provider(id)
	if err != nil {
		return TestResult{}, err
	}
	if err := p.KeyRefAllowed(); err != nil {
		return TestResult{Message: err.Error()}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	key, err := a.vault.Read(ctx, p.KeyRef)
	if err != nil {
		return TestResult{Message: "钥匙串中没有这个供应商的密钥"}, nil
	}
	defer clear(key)
	host, base, err := p.Endpoint()
	if err != nil {
		return TestResult{}, err
	}
	path := base + "/models"
	if p.Protocol == provider.Anthropic {
		path = base + "/v1/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		return TestResult{}, err
	}
	header, prefix := p.Injection()
	req.Header.Set(header, prefix+string(key))
	if p.Protocol == provider.Anthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	started := a.now()
	resp, err := a.client.Do(req)
	result := TestResult{LatencyMS: a.now().Sub(started).Milliseconds()}
	if err != nil {
		result.Message = "无法连接到 " + host + "（" + networkReason(err) + "）"
		return result, nil
	}
	resp.Body.Close()
	result.Status = resp.StatusCode
	switch {
	case resp.StatusCode == 200:
		result.OK, result.Message = true, "连接正常，密钥有效"
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		result.Message = fmt.Sprintf("服务返回了跳转（%d）。为保护密钥，AIofCage 不跟随跳转，请检查接口地址", resp.StatusCode)
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		result.Message = "服务可达，但密钥无效或没有权限"
	case resp.StatusCode == 404 || resp.StatusCode == 405:
		result.OK, result.Message = true, "服务可达（该服务不提供模型列表接口，无法在这里校验密钥）"
	case resp.StatusCode == 429:
		result.OK, result.Message = true, "服务可达，但当前被限流"
	default:
		result.Message = fmt.Sprintf("服务返回了 %d", resp.StatusCode)
	}
	return result, nil
}

func networkReason(err error) string {
	text := err.Error()
	switch {
	case strings.Contains(text, "Client.Timeout") || strings.Contains(text, "deadline exceeded") || strings.Contains(text, "timeout"):
		return "超时"
	case strings.Contains(text, "no such host"):
		return "域名无法解析"
	case strings.Contains(text, "certificate"):
		return "证书校验失败"
	case strings.Contains(text, "connection refused"):
		return "连接被拒绝"
	}
	return "网络错误"
}

// SandboxView is what the sandbox list shows.
type SandboxView struct {
	DemoCase       string              `json:"demo_case,omitempty"`
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	ProjectPath    string              `json:"project_path"`
	State          string              `json:"state"`
	Bindings       []BindingView       `json:"bindings"`
	Limits         sandbox.Limits      `json:"limits"`
	Usage          sandbox.Usage       `json:"usage"`
	MCP            []string            `json:"mcp"`
	MCPPermissions map[string][]string `json:"mcp_permissions,omitempty"`
	Filesystem     bool                `json:"filesystem_mcp"`
	DevSources     []string            `json:"dev_sources"`
	CreatedAt      time.Time           `json:"created_at"`
	// Operation is the lifecycle operation in progress (启动、停止、删除) and Progress its latest step.
	Operation     string `json:"operation,omitempty"`
	Progress      string `json:"progress,omitempty"`
	ConfigPending bool   `json:"config_pending"`
}

type BindingView struct {
	Agent              session.Agent `json:"agent"`
	ProviderID         string        `json:"provider_id"`
	ProviderName       string        `json:"provider_name"`
	Version            string        `json:"version"`
	TargetVersion      string        `json:"target_version"`
	DefaultVersion     string        `json:"default_version"`
	ActiveProviderID   string        `json:"active_provider_id"`
	ActiveProviderName string        `json:"active_provider_name"`
}

// effectiveGrants returns the grants that apply: a remote service's grant made for another
// address is left out, so the settings show that service's defaults instead.
func effectiveGrants(sbx sandbox.Sandbox, services []mcp.Service) map[string][]string {
	result := cloneMCPPermissions(sbx.MCPPermissions)
	for _, service := range services {
		if _, ok := result[service.ID]; ok {
			if _, explicit := runtime.Grant(sbx, service); !explicit {
				delete(result, service.ID)
			}
		}
	}
	return result
}

func cloneMCPPermissions(input map[string][]string) map[string][]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string][]string, len(input))
	for id, tools := range input {
		output[id] = append([]string{}, tools...)
	}
	return output
}

func (a *App) ListSandboxes() ([]SandboxView, error) {
	defaults, err := runtime.DefaultAgentVersions(a.store)
	if err != nil {
		return nil, err
	}
	sandboxes, err := a.store.Sandboxes()
	if err != nil {
		return nil, err
	}
	providers, err := a.store.Providers()
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, p := range providers {
		names[p.ID] = p.Name
	}
	services, err := a.store.MCPServices()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	views := make([]SandboxView, 0, len(sandboxes))
	for _, s := range sandboxes {
		view := SandboxView{ID: s.ID, Name: s.Name, ProjectPath: s.ProjectPath, DemoCase: s.DemoCase, State: "unknown",
			Limits: s.Limits, Usage: s.Usage, CreatedAt: s.CreatedAt, Bindings: []BindingView{},
			MCP: append([]string{}, s.MCP...), MCPPermissions: effectiveGrants(s, services), Filesystem: s.FilesystemMCP,
			DevSources: append([]string{}, s.DevSources...)}
		if a.manager != nil {
			view.State = a.manager.Status(ctx, s.ID).State
		}
		view.Operation, view.Progress = a.ops.get(s.ID)
		for _, b := range s.Bindings {
			activeID := b.ProviderID
			if s.AppliedProviders != nil {
				activeID = ""
				for _, ab := range s.AppliedBindings {
					if ab.Agent == b.Agent {
						activeID = ab.ProviderID
					}
				}
			}
			target := runtime.PinnedAgentVersions(s)[b.Agent]
			installed := s.AppliedAgentVersions[b.Agent]
			if installed == "" && view.State != "not_created" {
				installed = runtime.PinnedAgentVersions(sandbox.Sandbox{Bindings: []sandbox.Binding{b}})[b.Agent]
			}
			view.Bindings = append(view.Bindings, BindingView{Agent: b.Agent, ProviderID: b.ProviderID, ProviderName: names[b.ProviderID], Version: installed, TargetVersion: target, DefaultVersion: defaults[b.Agent], ActiveProviderID: activeID, ActiveProviderName: names[activeID]})
			if view.State == "running" && s.AppliedProviders != nil {
				if activeID != b.ProviderID {
					view.ConfigPending = true
				}
				if p, ok := s.AppliedProviders[b.ProviderID]; ok {
					for _, current := range providers {
						if current.ID == p.ID && (current.BaseURL != p.BaseURL || current.Model != p.Model || current.Protocol != p.Protocol || current.Auth != p.Auth || current.KeyRef != p.KeyRef || current.ImageInput != p.ImageInput || !samePricing(current.Pricing, p.Pricing)) {
							view.ConfigPending = true
						}
					}
				}
			}
		}
		views = append(views, view)
	}
	return views, nil
}

// DuplicateProvider 在可信宿主复制配置和凭据，前端不接触已保存密钥。
func (a *App) DuplicateProvider(id string) (ProviderView, error) {
	p, err := a.store.Provider(id)
	if err != nil {
		return ProviderView{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	key, err := a.vault.Read(ctx, p.KeyRef)
	if err != nil {
		return ProviderView{}, errors.New("原供应商的密钥不可用，无法复制")
	}
	defer clear(key)
	managed := p.KeyRef == provider.KeyRefFor(p.ID)
	p.ID, p.CreatedAt = newProviderID(), a.now().UTC()
	p.Name = string([]rune(p.Name)[:min(len([]rune(p.Name)), 55)]) + " 副本"
	if !managed {
		// A preset Keychain item stays bound to the preset's host. The copy refers to the same item
		// instead of cloning the key into an item of its own that could then be sent anywhere.
		if err := a.store.SaveProvider(p); err != nil {
			return ProviderView{}, err
		}
		return a.view(p, nil), nil
	}
	p.KeyRef = provider.KeyRefFor(p.ID)
	if err := a.vault.Put(ctx, p.KeyRef, key); err != nil {
		return ProviderView{}, err
	}
	if err := a.store.SaveProvider(p); err != nil {
		_ = a.vault.Delete(ctx, p.KeyRef)
		return ProviderView{}, err
	}
	return a.view(p, nil), nil
}

func samePricing(a, b *provider.Pricing) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
