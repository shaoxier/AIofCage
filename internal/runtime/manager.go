package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shaoxier/AIofCage/internal/events"
	"github.com/shaoxier/AIofCage/internal/gatekeeper"
	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/oauth"
	"github.com/shaoxier/AIofCage/internal/policy"
	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
	"github.com/shaoxier/AIofCage/internal/store"
	"github.com/shaoxier/AIofCage/internal/vault"
)

const (
	// specVersion changes whenever the docker create arguments below change, so that existing
	// containers are recreated (the home volume and project are kept).
	specVersion  = "2"
	specLabel    = "io.aiofcage.spec"
	labelKey     = "io.aiofcage.sandbox"
	tokenPath    = "/run/aiofcage/admin-token"
	agentUser    = "1000:1000"
	stateRunning = "running"
	stateStopped = "stopped"
	statePartial = "partial"
	stateMissing = "not_created"
)

// Manager owns sandbox lifecycles in the dedicated VM: one internal network, one egress network,
// one gatekeeper and one agent container per sandbox, plus one connector container for each
// enabled MCP service whose secret must stay out of the sandbox. It never runs agents on the host.
type Manager struct {
	env    *Environment
	docker runner
	store  *store.Store
	vault  vault.Store

	// Progress, when set, receives human-readable steps for long operations.
	Progress func(sandboxID, message string)
	// BeforeReplace 仅在候选镜像就绪、即将中断旧容器时关闭桌面会话。
	BeforeReplace func(sandboxID string)
	// OAuth refreshes tokens of OAuth MCP services before they are handed to a gatekeeper.
	OAuth *oauth.Client

	mu              sync.Mutex
	environmentMu   sync.RWMutex
	gates           map[string]*gateClient
	locks           map[string]*sync.Mutex
	connectorTokens map[string]string
}

func NewManager(st *store.Store, secrets vault.Store) (*Manager, error) {
	env, err := NewEnvironment(st)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil, TLSHandshakeTimeout: 10 * time.Second}}
	return &Manager{env: env, docker: env.docker, store: st, vault: secrets, OAuth: oauth.NewClient(client, nil),
		gates: map[string]*gateClient{}, locks: map[string]*sync.Mutex{}, connectorTokens: map[string]string{}}, nil
}

func (m *Manager) Environment() *Environment { return m.env }

func (m *Manager) progress(id, message string) {
	if m.Progress != nil {
		m.Progress(id, message)
	}
}

// lock 按沙箱串行化生命周期操作；同进程与跨进程的等待都响应取消。
func (m *Manager) lock(ctx context.Context, id string) (func(), error) {
	m.mu.Lock()
	l := m.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	m.mu.Unlock()
	for !l.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	unlock, err := m.store.LockOperation(ctx, "sandbox:"+id, true)
	if err != nil {
		l.Unlock()
		return nil, err
	}
	return func() { unlock(); l.Unlock() }, nil
}

// 环境共享锁覆盖整个启动或镜像替换，避免另一进程在检查空闲后同时停止虚拟机。
func (m *Manager) useEnvironment(ctx context.Context, exclusive bool) (func(), error) {
	if m.store == nil {
		return func() {}, nil
	}
	return m.store.LockOperation(ctx, "environment", exclusive)
}

type objectNames struct{ id, gate, agent, internal, egress, home string }

func names(id string) objectNames {
	prefix := "aiofcage-" + id
	return objectNames{id, prefix + "-gate", prefix + "-agent", prefix + "-in", prefix + "-out", prefix + "-home"}
}

type CreateRequest struct {
	Name           string              `json:"name"`
	ProjectPath    string              `json:"project_path"`
	Bindings       []sandbox.Binding   `json:"bindings"`
	Limits         sandbox.Limits      `json:"limits"`
	MCP            []string            `json:"mcp"`
	MCPPermissions map[string][]string `json:"mcp_permissions"`
	FilesystemMCP  bool                `json:"filesystem_mcp"`
}

// Create records a sandbox. Docker objects are created on first start.
func (m *Manager) Create(_ context.Context, request CreateRequest) (sandbox.Sandbox, error) {
	sbx := sandbox.Sandbox{ID: sandbox.NewID(), Name: strings.TrimSpace(request.Name),
		ProjectPath: filepath.Clean(request.ProjectPath), Bindings: request.Bindings, Limits: request.Limits,
		FilesystemMCP: request.FilesystemMCP, MCPPermissions: cloneMCPPermissions(request.MCPPermissions),
		Placeholders: map[string]string{}, CreatedAt: time.Now().UTC()}
	if err := sbx.Validate(); err != nil {
		return sbx, err
	}
	defaults, err := DefaultAgentVersions(m.store)
	if err != nil {
		return sbx, err
	}
	sbx.AgentVersions = map[session.Agent]string{}
	for _, agent := range sbx.Agents() {
		sbx.AgentVersions[agent] = defaults[agent]
	}
	if err := m.checkServices(request.MCP); err != nil {
		return sbx, err
	}
	permissions, err := m.normalizeMCPPermissions(request.MCP, request.MCPPermissions)
	if err != nil {
		return sbx, err
	}
	applyMCP(&sbx, request.MCP, permissions)
	for _, binding := range sbx.Bindings {
		if binding.ProviderID == "" {
			continue
		}
		if err := m.checkBinding(binding); err != nil {
			return sbx, err
		}
		if sbx.Placeholders[binding.ProviderID] == "" {
			sbx.Placeholders[binding.ProviderID] = sandbox.NewPlaceholder()
		}
	}
	return sbx, m.store.SaveSandbox(sbx)
}

func (m *Manager) checkServices(ids []string) error {
	for _, id := range ids {
		if _, err := m.store.MCPService(id); err != nil {
			return err
		}
	}
	return nil
}

// applyMCP sets the enabled MCP services, creating placeholders for new ones. It must not touch
// the store: SetMCP runs it inside a store update, whose lock is not reentrant.
func applyMCP(sbx *sandbox.Sandbox, ids []string, permissions map[string][]string) {
	var enabled []string
	for _, id := range ids {
		if sbx.Placeholders[mcp.PlaceholderKey(id)] == "" {
			sbx.Placeholders[mcp.PlaceholderKey(id)] = sandbox.NewPlaceholder()
		}
		enabled = append(enabled, id)
	}
	sbx.MCP = enabled
	sbx.MCPPermissions = cloneMCPPermissions(permissions)
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

func (m *Manager) normalizeMCPPermissions(ids []string, requested map[string][]string) (map[string][]string, error) {
	permissions := make(map[string][]string)
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
		service, err := m.store.MCPService(id)
		if err != nil {
			return nil, err
		}
		if !service.IsConnector() {
			continue
		}
		tools, specified := requested[id]
		if !specified {
			tools = service.Tools()
		}
		if err := mcp.ValidateTools(service, tools); err != nil {
			return nil, err
		}
		permissions[id] = append([]string{}, tools...)
	}
	for id := range requested {
		if !selected[id] {
			return nil, fmt.Errorf("MCP 权限引用了未启用的服务：%s", id)
		}
	}
	return permissions, nil
}

// SetMCP changes which MCP services a sandbox can use; a running sandbox is updated at once.
func (m *Manager) SetMCP(ctx context.Context, id string, ids []string, filesystem bool) error {
	return m.SetMCPPermissions(ctx, id, ids, filesystem, nil)
}

// SetMCPPermissions changes enabled MCP services and their connector tool grants.
func (m *Manager) SetMCPPermissions(ctx context.Context, id string, ids []string, filesystem bool, requested map[string][]string) error {
	unlock, lockErr := m.lock(ctx, id)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	if err := m.checkServices(ids); err != nil {
		return err
	}
	permissions, err := m.normalizeMCPPermissions(ids, requested)
	if err != nil {
		return err
	}
	err = m.store.UpdateSandbox(id, func(s *sandbox.Sandbox) error {
		if s.Placeholders == nil {
			s.Placeholders = map[string]string{}
		}
		s.FilesystemMCP = filesystem
		applyMCP(s, ids, permissions)
		return nil
	})
	if err != nil {
		return err
	}
	sbx, err := m.store.Sandbox(id)
	if err != nil {
		return err
	}
	return m.refresh(ctx, sbx)
}

func (m *Manager) servicesFor(sbx sandbox.Sandbox) (map[string]mcp.Service, error) {
	result := map[string]mcp.Service{}
	for _, id := range sbx.MCP {
		service, err := m.store.MCPService(id)
		if err != nil {
			return nil, err
		}
		result[id] = service
	}
	return result, nil
}

func (m *Manager) checkBinding(binding sandbox.Binding) error {
	p, err := m.store.Provider(binding.ProviderID)
	if err != nil {
		return err
	}
	if p.Agent != binding.Agent {
		return fmt.Errorf("供应商 %s 属于 %s，不能给 %s 使用", p.Name, p.Agent, binding.Agent)
	}
	return nil
}

// State is the observed state of a sandbox's containers.
type State struct {
	State        string `json:"state"`
	GateRunning  bool   `json:"gate_running"`
	AgentRunning bool   `json:"agent_running"`
}

func (m *Manager) Status(ctx context.Context, id string) State {
	n := names(id)
	gate := m.container(ctx, n.gate)
	agent := m.container(ctx, n.agent)
	state := State{GateRunning: gate.running, AgentRunning: agent.running}
	switch {
	case !gate.exists && !agent.exists:
		state.State = stateMissing
	case gate.running && agent.running:
		state.State = stateRunning
	case !gate.running && !agent.running:
		state.State = stateStopped
	default:
		state.State = statePartial
	}
	return state
}

type containerInfo struct {
	exists, running bool
	image, spec     string
}

func (m *Manager) container(ctx context.Context, name string) containerInfo {
	out, err := m.docker.run(ctx, 15*time.Second, nil, "container", "inspect", "--format",
		`{{.State.Running}} {{.Config.Image}} {{index .Config.Labels "`+specLabel+`"}}`, name)
	if err != nil {
		return containerInfo{}
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return containerInfo{}
	}
	info := containerInfo{exists: true, running: fields[0] == "true", image: fields[1]}
	if len(fields) > 2 {
		info.spec = fields[2]
	}
	return info
}

func (m *Manager) providersFor(sbx sandbox.Sandbox) (map[string]provider.Provider, error) {
	result := map[string]provider.Provider{}
	for _, binding := range sbx.Bindings {
		if binding.ProviderID == "" {
			continue
		}
		p, err := m.store.Provider(binding.ProviderID)
		if err != nil {
			return nil, err
		}
		result[p.ID] = p
	}
	return result, nil
}

// Start brings the sandbox up: VM, images, networks, gatekeeper (configured before the agent runs),
// then the agent with the gatekeeper CA and agent settings.
func (m *Manager) Start(ctx context.Context, id string) error {
	m.environmentMu.RLock()
	defer m.environmentMu.RUnlock()
	unlockEnvironment, environmentErr := m.useEnvironment(ctx, false)
	if environmentErr != nil {
		return environmentErr
	}
	defer unlockEnvironment()
	unlock, lockErr := m.lock(ctx, id)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	return m.start(ctx, id)
}

func (m *Manager) start(ctx context.Context, id string) error {
	return m.startSnapshot(ctx, id, nil)
}

// 增装恢复使用中断前已生效的配置，待重启的选择仍保留在存储中。
func (m *Manager) startSnapshot(ctx context.Context, id string, original *sandbox.Sandbox) error {
	sbx, err := m.store.Sandbox(id)
	if err != nil {
		return err
	}
	var providers map[string]provider.Provider
	if original != nil && original.AppliedProviders != nil {
		sbx.Bindings = append([]sandbox.Binding{}, original.AppliedBindings...)
		providers = original.AppliedProviders
	} else {
		providers, err = m.providersFor(sbx)
		if err != nil {
			return err
		}
	}
	m.progress(sbx.ID, "检查专用虚拟机")
	if err := m.env.Ensure(ctx); err != nil {
		return err
	}
	gateBuild, err := gatekeeperContext()
	if err != nil {
		return err
	}
	agentBuild, err := agentContextVersions(sbx.Agents(), PinnedAgentVersions(sbx))
	if err != nil {
		return err
	}
	for _, build := range []buildContext{gateBuild, agentBuild} {
		if err := m.ensureImage(ctx, build, func(message string) { m.progress(sbx.ID, message) }); err != nil {
			return err
		}
	}
	m.progress(sbx.ID, "准备网络与容器")
	if err := m.ensureObjects(ctx, sbx, gateBuild.tag, agentBuild.tag); err != nil {
		return err
	}
	n := names(sbx.ID)
	m.progress(sbx.ID, "启动门卫")
	if _, err := m.docker.run(ctx, 30*time.Second, nil, "start", n.gate); err != nil {
		return err
	}
	client, err := m.connect(ctx, sbx.ID)
	if err != nil {
		m.stopContainers(context.Background(), n)
		return err
	}
	services, err := m.servicesFor(sbx)
	if err != nil {
		m.stopContainers(context.Background(), n)
		return err
	}
	routes, err := m.syncConnectors(ctx, sbx, services)
	if err != nil {
		m.stopContainers(context.Background(), n)
		return err
	}
	if err := m.configure(ctx, client, sbx, providers, routes); err != nil {
		m.stopContainers(context.Background(), n)
		return err
	}
	m.progress(sbx.ID, "启动 Agent 容器")
	if _, err := m.docker.run(ctx, 30*time.Second, nil, "start", n.agent); err != nil {
		m.stopContainers(context.Background(), n)
		return err
	}
	if err := m.writeAgentFiles(ctx, client, sbx, providers); err != nil {
		m.stopContainers(context.Background(), n)
		return err
	}
	if err := m.store.UpdateSandbox(id, func(s *sandbox.Sandbox) error {
		s.AppliedBindings = append([]sandbox.Binding{}, sbx.Bindings...)
		s.AppliedProviders = providers
		s.AgentVersions = PinnedAgentVersions(sbx)
		s.AppliedAgentVersions = PinnedAgentVersions(sbx)
		return nil
	}); err != nil {
		m.stopContainers(context.Background(), n)
		return err
	}
	m.progress(sbx.ID, "已启动")
	return nil
}

func (m *Manager) ensureObjects(ctx context.Context, sbx sandbox.Sandbox, gateImage, agentImage string) error {
	n := names(sbx.ID)
	label := labelKey + "=" + sbx.ID
	createdNetwork := false
	for _, network := range []struct {
		name     string
		internal bool
	}{{n.internal, true}, {n.egress, false}} {
		if _, err := m.docker.run(ctx, 15*time.Second, nil, "network", "inspect", network.name); err == nil {
			continue
		}
		args := []string{"network", "create", "--label", label}
		if network.internal {
			args = append(args, "--internal")
		}
		if _, err := m.docker.run(ctx, 30*time.Second, nil, append(args, network.name)...); err != nil {
			return err
		}
		createdNetwork = true
	}
	if createdNetwork {
		// Containers attached to a removed network cannot start again.
		if err := m.removeConnectors(ctx, sbx.ID); err != nil {
			return err
		}
	}
	proxyIP, err := m.hostAddress(ctx, n.internal)
	if err != nil {
		return err
	}
	adminIP, err := m.hostAddress(ctx, n.egress)
	if err != nil {
		return err
	}
	if _, err := m.docker.run(ctx, 15*time.Second, nil, "volume", "inspect", n.home); err != nil {
		if _, err := m.docker.run(ctx, 15*time.Second, nil, "volume", "create", "--label", label, n.home); err != nil {
			return err
		}
	}
	gate := m.container(ctx, n.gate)
	if gate.exists && (createdNetwork || gate.image != gateImage || gate.spec != specVersion) {
		if _, err := m.docker.run(ctx, 30*time.Second, nil, "rm", "--force", n.gate); err != nil {
			return err
		}
		gate.exists = false
	}
	if !gate.exists {
		if _, err := m.docker.run(ctx, 30*time.Second, nil, "create", "--name", n.gate, "--label", label, "--label", specLabel+"="+specVersion,
			"--network", n.egress, "--ip", adminIP, "--publish", "127.0.0.1::8899",
			"--read-only", "--tmpfs", "/run/aiofcage:uid=65532,gid=65532,mode=0700,size=1m",
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
			"--memory", "256m", "--cpus", "1", "--pids-limit", "128",
			gateImage, "--token-out", tokenPath,
			"--management-listen", adminIP+":8899", "--proxy-listen", proxyIP+":8888", "--mcp-listen", proxyIP+":8890"); err != nil {
			return err
		}
		if _, err := m.docker.run(ctx, 30*time.Second, nil, "network", "connect", "--ip", proxyIP,
			"--alias", "gatekeeper", n.internal, n.gate); err != nil {
			return err
		}
	}
	agent := m.container(ctx, n.agent)
	if agent.exists && (createdNetwork || agent.image != agentImage || agent.spec != specVersion) {
		if _, err := m.docker.run(ctx, 30*time.Second, nil, "rm", "--force", n.agent); err != nil {
			return err
		}
		agent.exists = false
	}
	if !agent.exists {
		if err := sandbox.ValidateProject(sbx.ProjectPath); err != nil {
			return err
		}
		if _, err := m.docker.run(ctx, 30*time.Second, nil, "create", "--name", n.agent, "--label", label, "--label", specLabel+"="+specVersion,
			"--hostname", "aiofcage", "--network", n.internal, "--dns", "127.0.0.1", "--init",
			"--user", agentUser, "--read-only",
			// exec: compilers, test runners and native addons (dsh) run binaries from /tmp.
			"--tmpfs", "/tmp:exec,uid=1000,gid=1000,mode=1777,size=1g",
			"--tmpfs", "/etc/ssl/certs:uid=1000,gid=1000,mode=0755,size=4m",
			"--mount", "type=volume,src="+n.home+",dst=/home/agent",
			"--mount", "type=bind,src="+sbx.ProjectPath+",dst="+sbx.ProjectPath,
			"--workdir", sbx.ProjectPath,
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
			"--memory", "4g", "--cpus", "2", "--pids-limit", "512",
			agentImage); err != nil {
			return err
		}
	}
	return nil
}

// hostAddress picks .2 in the network's IPv4 subnet for the gatekeeper (Docker uses .1 as gateway).
func (m *Manager) hostAddress(ctx context.Context, network string) (string, error) {
	out, err := m.docker.run(ctx, 15*time.Second, nil, "network", "inspect", "--format", "{{json .IPAM.Config}}", network)
	if err != nil {
		return "", err
	}
	var config []struct {
		Subnet string `json:"Subnet"`
	}
	if err := json.Unmarshal(out, &config); err != nil {
		return "", errors.New("无法读取网络地址段")
	}
	for _, entry := range config {
		prefix, err := netip.ParsePrefix(entry.Subnet)
		if err != nil || !prefix.Addr().Is4() || prefix.Bits() > 29 {
			continue
		}
		return prefix.Masked().Addr().Next().Next().String(), nil
	}
	return "", fmt.Errorf("网络 %s 没有可用的 IPv4 地址段", network)
}

// connect reads the gatekeeper's self-generated token and loopback port. The token never leaves
// the host process; it is not stored on disk.
func (m *Manager) connect(ctx context.Context, id string) (*gateClient, error) {
	n := names(id)
	var token, port string
	for attempt := 0; attempt < 50; attempt++ {
		if token == "" {
			if out, err := m.docker.run(ctx, 10*time.Second, nil, "exec", n.gate, "cat", tokenPath); err == nil {
				token = strings.TrimSpace(string(out))
			}
		}
		if port == "" {
			if out, err := m.docker.run(ctx, 10*time.Second, nil, "port", n.gate, "8899/tcp"); err == nil {
				for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
					if strings.HasPrefix(line, "127.0.0.1:") {
						port = strings.TrimPrefix(line, "127.0.0.1:")
					}
				}
			}
		}
		if token != "" && port != "" {
			client := newGateClient(port, token)
			if _, err := client.health(ctx); err == nil {
				m.mu.Lock()
				m.gates[id] = client
				m.mu.Unlock()
				return client, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil, errors.New("门卫没有就绪（无法读取管理凭据或连接管理端口）")
}

// client returns a connection to a running gatekeeper, reconnecting after an app restart.
func (m *Manager) client(ctx context.Context, id string) (*gateClient, error) {
	m.mu.Lock()
	client := m.gates[id]
	m.mu.Unlock()
	if client != nil {
		if _, err := client.health(ctx); err == nil {
			return client, nil
		}
	}
	if !m.container(ctx, names(id).gate).running {
		return nil, errors.New("沙箱未运行")
	}
	return m.connect(ctx, id)
}

func (m *Manager) gateConfig(ctx context.Context, sbx sandbox.Sandbox, providers map[string]provider.Provider,
	connectors []gatekeeper.Connector) (gatekeeper.Config, error) {
	config := gatekeeper.Config{Connectors: connectors, Limits: gatekeeper.Limits{
		MaxRequests: sbx.Limits.MaxRequests, MaxTokens: sbx.Limits.MaxTokens,
		BaselineRequests: sbx.Usage.Requests, BaselineTokens: sbx.Usage.Tokens}}
	config.Policy.Version = uint64(time.Now().UnixMilli())
	config.Policy.SessionID = sbx.ID
	for _, id := range sortedKeys(providers) {
		p := providers[id]
		if err := p.KeyRefAllowed(); err != nil {
			return config, fmt.Errorf("供应商「%s」：%w", p.Name, err)
		}
		key, err := m.vault.Read(ctx, p.KeyRef)
		if err != nil {
			return config, fmt.Errorf("供应商「%s」的密钥不可用：%w", p.Name, err)
		}
		header, prefix := p.Injection()
		placeholder := sbx.Placeholders[p.ID]
		if placeholder == "" {
			return config, fmt.Errorf("供应商「%s」缺少占位符", p.Name)
		}
		config.Secrets = append(config.Secrets, gatekeeper.Secret{ID: p.ID, Pricing: p.Pricing, Model: p.Model, Agent: string(p.Agent), Name: p.Name, Placeholder: placeholder,
			Header: header, Prefix: prefix, Value: string(key)})
		config.Policy.Rules = append(config.Policy.Rules, p.Rules()...)
	}
	services, err := m.servicesFor(sbx)
	if err != nil {
		return config, err
	}
	for _, id := range sortedKeys(services) {
		service := services[id]
		if service.IsConnector() {
			continue // its secret goes to the connector, not to the gatekeeper
		}
		token, err := m.serviceToken(ctx, service)
		if err != nil && service.IsOAuth() {
			// An expired login is routine: the sandbox still starts, only this service stays closed.
			m.progress(sbx.ID, fmt.Sprintf("MCP 服务「%s」需要重新登录授权，本次先不开放（%v）", service.Name, err))
			continue
		}
		if err != nil {
			return config, fmt.Errorf("MCP 服务「%s」的令牌不可用：%w", service.Name, err)
		}
		placeholder := sbx.Placeholders[mcp.PlaceholderKey(id)]
		if placeholder == "" {
			return config, fmt.Errorf("MCP 服务「%s」缺少占位符", service.Name)
		}
		config.Secrets = append(config.Secrets, gatekeeper.Secret{ID: mcp.SecretID(id), Placeholder: placeholder,
			Header: "Authorization", Prefix: "Bearer ", Value: string(token)})
		config.Policy.Rules = append(config.Policy.Rules, service.Rules()...)
	}
	return config, nil
}

// OAuthMargin is how long an OAuth access token must stay valid when handed to a gatekeeper.
const OAuthMargin = 5 * time.Minute

// serviceToken returns the bearer token of a remote MCP service. OAuth tokens are refreshed on
// the host first when they would expire soon; refresh tokens never leave the host.
func (m *Manager) serviceToken(ctx context.Context, service mcp.Service) (string, error) {
	if service.IsOAuth() {
		creds, _, err := m.OAuth.AccessToken(ctx, m.vault, service.KeyRef, OAuthMargin)
		if err != nil {
			return "", err
		}
		return creds.AccessToken, nil
	}
	token, err := m.vault.Read(ctx, service.KeyRef)
	return string(token), err
}

func (m *Manager) configure(ctx context.Context, client *gateClient, sbx sandbox.Sandbox, providers map[string]provider.Provider,
	connectors []gatekeeper.Connector) error {
	m.progress(sbx.ID, "向门卫下发策略与密钥")
	config, err := m.gateConfig(ctx, sbx, providers, connectors)
	if err != nil {
		return err
	}
	err = client.putConfig(ctx, config)
	for i := range config.Secrets {
		config.Secrets[i].Value = ""
	}
	return err
}

func (m *Manager) writeAgentFiles(ctx context.Context, client *gateClient, sbx sandbox.Sandbox, providers map[string]provider.Provider) error {
	ca, err := client.ca(ctx)
	if err != nil {
		return err
	}
	services, err := m.servicesFor(sbx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		CA    string      `json:"ca"`
		Files []agentFile `json:"files"`
	}{ca, agentFiles(sbx, providers, services)})
	if err != nil {
		return err
	}
	if _, err := m.docker.run(ctx, 30*time.Second, payload, "exec", "--interactive", "--user", agentUser,
		names(sbx.ID).agent, "python3", "/opt/aiofcage/aiofcage-init.py"); err != nil {
		return fmt.Errorf("写入 Agent 配置失败：%w", err)
	}
	return nil
}

// Bind 只保存选择；当前容器的配置保留到下次重启。
func (m *Manager) Bind(ctx context.Context, id string, agent session.Agent, providerID string) error {
	unlock, lockErr := m.lock(ctx, id)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	if err := m.captureApplied(ctx, id); err != nil {
		return err
	}
	if providerID != "" {
		if err := m.checkBinding(sandbox.Binding{Agent: agent, ProviderID: providerID}); err != nil {
			return err
		}
	}
	err := m.store.UpdateSandbox(id, func(s *sandbox.Sandbox) error {
		found := false
		for i := range s.Bindings {
			if s.Bindings[i].Agent == agent {
				s.Bindings[i].ProviderID, found = providerID, true
			}
		}
		if !found {
			return fmt.Errorf("沙箱里没有安装 %s", agent)
		}
		if s.Placeholders == nil {
			s.Placeholders = map[string]string{}
		}
		if providerID != "" && s.Placeholders[providerID] == "" {
			s.Placeholders[providerID] = sandbox.NewPlaceholder()
		}
		return nil
	})
	return err
}

// Refresh 刷新策略与 Agent 设置，但继续使用已生效的供应商快照；新选择在启动时应用。
func (m *Manager) Refresh(ctx context.Context, id string) error {
	unlock, lockErr := m.lock(ctx, id)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	sbx, err := m.store.Sandbox(id)
	if err != nil {
		return err
	}
	return m.refresh(ctx, sbx)
}

// captureApplied 为升级前的运行容器补记配置，避免修改时误称已经生效。
func (m *Manager) captureApplied(ctx context.Context, id string) error {
	sbx, err := m.store.Sandbox(id)
	if err != nil {
		return err
	}
	if sbx.AppliedProviders != nil || m.Status(ctx, id).State != stateRunning {
		return nil
	}
	providers, err := m.providersFor(sbx)
	if err != nil {
		return err
	}
	return m.store.UpdateSandbox(id, func(s *sandbox.Sandbox) error {
		s.AppliedBindings = append([]sandbox.Binding{}, sbx.Bindings...)
		s.AppliedProviders = providers
		return nil
	})
}

// CaptureApplied 供供应商编辑在保存新配置前记录现有配置。
func (m *Manager) CaptureApplied(ctx context.Context, id string) error {
	unlock, lockErr := m.lock(ctx, id)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	return m.captureApplied(ctx, id)
}

func (m *Manager) refresh(ctx context.Context, sbx sandbox.Sandbox) error {
	if m.Status(ctx, sbx.ID).State != stateRunning {
		return nil
	}
	if sbx.AppliedProviders != nil {
		sbx.Bindings = sbx.AppliedBindings
	}
	providers, err := m.providersFor(sbx)
	if err != nil {
		return err
	}
	if sbx.AppliedProviders != nil {
		providers = sbx.AppliedProviders
	}
	client, err := m.client(ctx, sbx.ID)
	if err != nil {
		return err
	}
	services, err := m.servicesFor(sbx)
	if err != nil {
		return err
	}
	routes, err := m.syncConnectors(ctx, sbx, services)
	if err != nil {
		return err
	}
	if err := m.configure(ctx, client, sbx, providers, routes); err != nil {
		return err
	}
	return m.writeAgentFiles(ctx, client, sbx, providers)
}

// Stop saves cumulative usage so quotas survive restarts, revokes the live gatekeeper policy,
// removes connector containers that may still hold secrets, then stops both containers.
func (m *Manager) Stop(ctx context.Context, id string) error {
	unlock, lockErr := m.lock(ctx, id)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	sbx, err := m.store.Sandbox(id)
	if err != nil {
		return err
	}
	m.saveUsage(ctx, sbx)
	var errs []error
	if err := m.revokeRuntime(ctx, sbx); err != nil {
		errs = append(errs, err)
	}
	if err := m.stopContainers(ctx, names(sbx.ID)); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return m.store.UpdateSandbox(id, func(s *sandbox.Sandbox) error { s.AppliedBindings = nil; s.AppliedProviders = nil; return nil })
}

// revokeRuntime invalidates the in-memory gatekeeper policy before stopping the sandbox and
// removes connector containers so database passwords and connector launch state do not survive a
// stop. The next start creates fresh connector tokens and delivers secrets again.
func (m *Manager) revokeRuntime(ctx context.Context, sbx sandbox.Sandbox) error {
	var errs []error
	client, err := m.client(ctx, sbx.ID)
	if err == nil {
		config := gatekeeper.Config{Policy: policy.Snapshot{Version: uint64(time.Now().UnixMilli()), SessionID: sbx.ID}}
		if err := client.putConfig(ctx, config); err != nil {
			errs = append(errs, fmt.Errorf("清空沙箱「%s」的门卫授权失败：%w", sbx.Name, err))
		}
	} else if m.Status(ctx, sbx.ID).State == stateRunning {
		errs = append(errs, fmt.Errorf("读取沙箱「%s」的门卫连接失败：%w", sbx.Name, err))
	}
	if err := m.removeConnectors(ctx, sbx.ID); err != nil {
		errs = append(errs, fmt.Errorf("清理沙箱「%s」的 MCP 连接器失败：%w", sbx.Name, err))
	}
	m.mu.Lock()
	delete(m.gates, sbx.ID)
	m.mu.Unlock()
	m.revokeRuntimeTokens(sbx.ID)
	return errors.Join(errs...)
}

func (m *Manager) revokeRuntimeTokens(sandboxID string) {
	prefix := "aiofcage-" + sandboxID + "-mcp-"
	m.mu.Lock()
	defer m.mu.Unlock()
	for name := range m.connectorTokens {
		if strings.HasPrefix(name, prefix) {
			delete(m.connectorTokens, name)
		}
	}
}

func (m *Manager) saveUsage(ctx context.Context, sbx sandbox.Sandbox) {
	client, err := m.client(ctx, sbx.ID)
	if err != nil {
		return
	}
	usage, err := client.usage(ctx)
	if err != nil {
		return
	}
	_ = m.RecordUsage(sbx.ID, usage)
}

// RecordUsage folds a live gatekeeper usage snapshot into the stored cumulative counters.
func (m *Manager) RecordUsage(id string, usage GateUsage) error {
	return m.store.UpdateSandbox(id, func(s *sandbox.Sandbox) error {
		s.AddLive(usage.RunID, usage.Providers(), usage.QuotaRequests, usage.QuotaTokens)
		return nil
	})
}

func (m *Manager) stopContainers(ctx context.Context, n objectNames) error {
	var errs []error
	connectors, err := m.listConnectors(ctx, n.id)
	if err != nil {
		errs = append(errs, err)
	}
	// Agent first: it must never outlive its gatekeeper. Connectors stop before the gatekeeper too.
	for _, name := range append(append([]string{n.agent}, connectors...), n.gate) {
		if info := m.container(ctx, name); info.running {
			if _, err := m.docker.run(ctx, 30*time.Second, nil, "stop", "--time", "5", name); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Reset tears down a sandbox's runtime objects while keeping its stored configuration and
// project directory. When restart is true, the normal start path recreates the VM objects and
// reapplies the saved configuration after the teardown succeeds.
func (m *Manager) Reset(ctx context.Context, id string, keepHome, restart bool) error {
	m.environmentMu.RLock()
	defer m.environmentMu.RUnlock()
	unlockEnvironment, environmentErr := m.useEnvironment(ctx, false)
	if environmentErr != nil {
		return environmentErr
	}
	defer unlockEnvironment()
	unlock, lockErr := m.lock(ctx, id)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	sbx, err := m.store.Sandbox(id)
	if err != nil {
		return err
	}
	m.saveUsage(ctx, sbx)
	var errs []error
	if err := m.revokeRuntime(ctx, sbx); err != nil {
		errs = append(errs, err)
	}
	if err := m.stopContainers(ctx, names(sbx.ID)); err != nil {
		errs = append(errs, err)
	}
	if err := m.removeRuntimeObjects(ctx, sbx.ID, keepHome); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if err := m.store.UpdateSandbox(id, func(s *sandbox.Sandbox) error {
		s.AppliedBindings = nil
		s.AppliedProviders = nil
		s.AppliedAgentVersions = nil
		return nil
	}); err != nil {
		return err
	}
	if restart {
		return m.startSnapshot(ctx, id, nil)
	}
	return nil
}

// removeRuntimeObjects removes containers, networks and optionally the Agent home volume. The
// project directory and stored sandbox configuration are never touched.
func (m *Manager) removeRuntimeObjects(ctx context.Context, id string, keepHome bool) error {
	n := names(id)
	var errs []error
	if err := m.removeConnectors(ctx, id); err != nil {
		errs = append(errs, err)
	}
	for _, args := range [][]string{{"rm", "--force", n.agent}, {"rm", "--force", n.gate},
		{"network", "rm", "--force", n.internal}, {"network", "rm", "--force", n.egress}} {
		if _, err := m.docker.run(ctx, 30*time.Second, nil, args...); err != nil && !strings.Contains(err.Error(), "No such") &&
			!strings.Contains(err.Error(), "not found") {
			errs = append(errs, err)
		}
	}
	if !keepHome {
		if _, err := m.docker.run(ctx, 30*time.Second, nil, "volume", "rm", "--force", n.home); err != nil &&
			!strings.Contains(err.Error(), "no such") && !strings.Contains(err.Error(), "No such") {
			errs = append(errs, err)
		}
	}
	m.mu.Lock()
	delete(m.gates, id)
	m.mu.Unlock()
	m.revokeRuntimeTokens(id)
	return errors.Join(errs...)
}

// Delete removes containers, networks and (unless keepHome) the agent home volume. The project
// directory is never touched.
func (m *Manager) Delete(ctx context.Context, id string, keepHome bool) error {
	unlock, lockErr := m.lock(ctx, id)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	sbx, err := m.store.Sandbox(id)
	if err != nil {
		return err
	}
	if err := m.removeRuntimeObjects(ctx, sbx.ID, keepHome); err != nil {
		return err
	}
	return m.store.DeleteSandbox(sbx.ID)
}

// Usage returns live counters from the gatekeeper.
func (m *Manager) Usage(ctx context.Context, id string) (GateUsage, error) {
	client, err := m.client(ctx, id)
	if err != nil {
		return GateUsage{}, err
	}
	return client.usage(ctx)
}

// Events 订阅脱敏事件；接收方确认失败时立即中断，以便从已持久化的游标续传。
func (m *Manager) Events(ctx context.Context, id, lastID string, handle events.Handler) error {
	client, err := m.client(ctx, id)
	if err != nil {
		return err
	}
	return client.events(ctx, lastID, handle)
}

// ExecArgs returns the docker arguments that run a command as the agent user inside the sandbox,
// with the agent settings loaded. Used for terminals and the CLI.
func (m *Manager) ExecArgs(sbx sandbox.Sandbox, tty bool, command ...string) []string {
	args := []string{m.docker.path, "--context", DockerContext, "exec", "--interactive"}
	if tty {
		args = append(args, "--tty")
	}
	args = append(args, "--user", agentUser, "--workdir", sbx.ProjectPath, names(sbx.ID).agent, "aiofcage-run")
	if len(command) == 0 {
		command = []string{"bash"}
	}
	return append(args, command...)
}

// Exec runs a command in the sandbox with the caller's stdio.
func (m *Manager) Exec(ctx context.Context, id string, tty bool, stdin io.Reader, stdout, stderr io.Writer, command ...string) error {
	sbx, err := m.store.Sandbox(id)
	if err != nil {
		return err
	}
	args := m.ExecArgs(sbx, tty, command...)
	return runner{path: args[0]}.stream(ctx, stdin, stdout, stderr, args[1:]...)
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// RunID returns the current gatekeeper run of a running sandbox (it changes on every restart).
func (m *Manager) RunID(ctx context.Context, id string) (string, error) {
	client, err := m.client(ctx, id)
	if err != nil {
		return "", err
	}
	health, err := client.health(ctx)
	return health.RunID, err
}

func (m *Manager) removeConnectors(ctx context.Context, sandboxID string) error {
	connectors, err := m.listConnectors(ctx, sandboxID)
	if err != nil {
		return err
	}
	for _, name := range connectors {
		if _, err := m.docker.run(ctx, 30*time.Second, nil, "rm", "--force", name); err != nil {
			return err
		}
		m.mu.Lock()
		delete(m.connectorTokens, name)
		m.mu.Unlock()
	}
	return nil
}

// TerminalCommand returns an unstarted command that runs command as the agent user inside the
// sandbox with a TTY, for the desktop app's built-in terminal. The caller attaches a PTY.
func (m *Manager) TerminalCommand(sbx sandbox.Sandbox, command ...string) *exec.Cmd {
	args := m.ExecArgs(sbx, true, command...)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = childEnv()
	return cmd
}
