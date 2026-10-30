package runtime

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/shaoxier/AIofCage/internal/connector"
	"github.com/shaoxier/AIofCage/internal/gatekeeper"
	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/store"
)

const (
	roleLabel        = "io.aiofcage.role"
	connectorRole    = "connector"
	connectorPort    = "8080"
	connectorUser    = "1000:1000"
	launchPath       = "/run/aiofcage/launch.json"
	connectorCommand = "/usr/local/bin/aiofcage-connector"
)

// connectorName is the container of one connector service in one sandbox.
func connectorName(sandboxID, serviceID string) string {
	return "aiofcage-" + sandboxID + "-mcp-" + serviceID
}

// Grant returns the tools a sandbox may call on a service, and whether they come from the
// sandbox's own grant. A remote service's grant applies only to the address it was made for; a
// grant without a recorded address (an older version rewrote the file without the field) is
// taken as made for the current one and gets bound at the next pin, so it is never widened.
// Without an applicable grant (a sandbox saved before grants existed, or a service that moved)
// the service's default tools apply until a grant is pinned (see prepareRemote).
func Grant(sbx sandbox.Sandbox, service mcp.Service) ([]string, bool) {
	tools, ok := sbx.MCPPermissions[service.ID]
	if bound, recorded := sbx.MCPGrantURLs[service.ID]; ok && recorded && !service.IsConnector() {
		upstream, err := service.Upstream()
		ok = err == nil && bound == upstream
	}
	if !ok {
		return service.DefaultTools(), false
	}
	return append([]string{}, tools...), true
}

func grantedTools(sbx sandbox.Sandbox, service mcp.Service) []string {
	tools, _ := Grant(sbx, service)
	return tools
}

// prepareRemote lists the tools of remote services whose tools were never listed, and pins the
// default grant of services the sandbox uses without an applicable grant once their tools are
// known: tools a service adds later must not be granted without the user choosing them. It runs
// under the sandbox's lifecycle lock, like every other writer of grants. services is updated.
func (m *Manager) prepareRemote(ctx context.Context, sbx sandbox.Sandbox, services map[string]mcp.Service) (sandbox.Sandbox, error) {
	type pin struct {
		tools []string
		url   string
	}
	pins := map[string]pin{}
	for _, id := range sortedKeys(services) {
		service := services[id]
		if service.IsConnector() {
			continue
		}
		if service.CatalogAt == nil && m.http != nil {
			m.progress(sbx.ID, "获取 MCP 服务「"+service.Name+"」的工具列表")
			if listed, err := m.ListTools(ctx, service); err != nil {
				m.progress(sbx.ID, fmt.Sprintf("没能获取「%s」的工具列表，本次不开放它的工具（%v）", service.Name, err))
			} else {
				service = listed
				services[id] = listed
			}
		}
		upstream, err := service.Upstream()
		if err != nil {
			continue
		}
		tools, explicit := Grant(sbx, service)
		_, bound := sbx.MCPGrantURLs[id]
		switch {
		case explicit && !bound:
			pins[id] = pin{tools: tools, url: upstream} // record the address the grant is now used for
		case !explicit && service.CatalogAt != nil:
			pins[id] = pin{tools: service.DefaultTools(), url: upstream}
		}
	}
	if len(pins) == 0 {
		return sbx, nil
	}
	apply := func(s *sandbox.Sandbox) {
		if s.MCPPermissions == nil {
			s.MCPPermissions = map[string][]string{}
		}
		if s.MCPGrantURLs == nil {
			s.MCPGrantURLs = map[string]string{}
		}
		for id, p := range pins {
			s.MCPPermissions[id], s.MCPGrantURLs[id] = append([]string{}, p.tools...), p.url
		}
	}
	if err := m.store.UpdateSandbox(sbx.ID, func(s *sandbox.Sandbox) error { apply(s); return nil }); err != nil {
		return sbx, err
	}
	sbx.MCPPermissions, sbx.MCPGrantURLs = cloneMCPPermissions(sbx.MCPPermissions), maps.Clone(sbx.MCPGrantURLs)
	apply(&sbx)
	return sbx, nil
}

// ListTools asks a remote service for its tools with the token the host holds and saves them as
// the service's tool list. Grants are not changed.
func (m *Manager) ListTools(ctx context.Context, service mcp.Service) (mcp.Service, error) {
	// The address and the token are read together: a copy of the service read earlier may hold an
	// address the service has since left, and the token saved now belongs to the new one.
	current, token, err := m.credential(ctx, service.ID)
	if err != nil {
		return service, err
	}
	upstream, err := current.Upstream()
	if err != nil {
		return service, err
	}
	listCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tools, err := mcp.ListTools(listCtx, m.http, upstream, token)
	if err != nil {
		return service, err
	}
	return SaveCatalog(m.store, current, tools, time.Now())
}

// ErrServiceChanged means a service was edited while its tools were being listed; the list
// belongs to the old address and is discarded.
var ErrServiceChanged = errors.New("MCP 服务在获取工具列表期间被修改了，请重新获取")

// SaveCatalog records the tools listed from service's address. Only the tool list is written,
// and only while the stored service still has that address and credential kind, so an edit
// made in the meantime is neither overwritten nor given the old server's tools.
func SaveCatalog(st *store.Store, service mcp.Service, tools []mcp.Tool, now time.Time) (mcp.Service, error) {
	listedFrom, err := service.Upstream()
	if err != nil {
		return service, err
	}
	stamp := now.UTC()
	var saved mcp.Service
	err = st.UpdateMCPService(service.ID, func(current *mcp.Service) error {
		upstream, err := current.Upstream()
		if err != nil || upstream != listedFrom || current.Auth != service.Auth || current.Kind != service.Kind {
			return ErrServiceChanged
		}
		current.Catalog, current.CatalogAt = tools, &stamp
		saved = *current
		return nil
	})
	return saved, err
}

func connectorServices(services map[string]mcp.Service) []mcp.Service {
	var result []mcp.Service
	for _, id := range sortedKeys(services) {
		if services[id].IsConnector() {
			result = append(result, services[id])
		}
	}
	return result
}

// listConnectors returns the names of a sandbox's connector containers, running or not.
func (m *Manager) listConnectors(ctx context.Context, sandboxID string) ([]string, error) {
	out, err := m.docker.run(ctx, 15*time.Second, nil, "ps", "--all", "--format", "{{.Names}}",
		"--filter", "label="+labelKey+"="+sandboxID, "--filter", "label="+roleLabel+"="+connectorRole)
	if err != nil {
		return nil, err
	}
	names := strings.Fields(string(out))
	sort.Strings(names)
	return names, nil
}

// syncConnectors makes a sandbox's connector containers match its enabled connector services,
// delivers each one its secret, and returns the gatekeeper routes to them. Connectors sit only on
// the sandbox's egress network: the gatekeeper can reach them, the agent cannot.
func (m *Manager) syncConnectors(ctx context.Context, sbx sandbox.Sandbox, services map[string]mcp.Service) ([]gatekeeper.Connector, error) {
	wanted := connectorServices(services)
	keep := map[string]bool{}
	for _, service := range wanted {
		keep[connectorName(sbx.ID, service.ID)] = true
	}
	existing, err := m.listConnectors(ctx, sbx.ID)
	if err != nil {
		return nil, err
	}
	for _, name := range existing {
		if !keep[name] {
			if _, err := m.docker.run(ctx, 30*time.Second, nil, "rm", "--force", name); err != nil {
				return nil, err
			}
			m.mu.Lock()
			delete(m.connectorTokens, name)
			m.mu.Unlock()
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	build, err := connectorContext()
	if err != nil {
		return nil, err
	}
	if err := m.ensureImage(ctx, build, func(message string) { m.progress(sbx.ID, message) }); err != nil {
		return nil, err
	}
	n := names(sbx.ID)
	var routes []gatekeeper.Connector
	for _, service := range wanted {
		m.progress(sbx.ID, "启动连接器「"+service.Name+"」")
		name := connectorName(sbx.ID, service.ID)
		if err := m.ensureConnector(ctx, sbx.ID, name, build.tag, n.egress); err != nil {
			return nil, err
		}
		var secret []byte
		if !service.IsBuiltin() {
			secret, err = m.vault.Read(ctx, service.KeyRef)
			if err != nil {
				return nil, fmt.Errorf("MCP 服务「%s」的密码不可用：%w", service.Name, err)
			}
		}
		command, redact, err := service.Launch(string(secret))
		clear(secret)
		if err != nil {
			return nil, err
		}
		token, err := m.connectorToken(name)
		if err != nil {
			return nil, err
		}
		payload, err := json.Marshal(connector.Launch{Token: token, Command: command, Redact: redact})
		if err != nil {
			return nil, err
		}
		// The secret travels on stdin into a tmpfs file the connector reads once and removes; it
		// never appears in docker inspect, environment variables or arguments.
		if _, err := m.docker.run(ctx, 30*time.Second, payload, "exec", "--interactive", "--user", connectorUser,
			name, connectorCommand, "deliver", launchPath); err != nil {
			return nil, fmt.Errorf("向连接器「%s」投递配置失败：%w", service.Name, err)
		}
		ip, err := m.containerIP(ctx, name, n.egress)
		if err != nil {
			return nil, err
		}
		placeholder := sbx.Placeholders[mcp.PlaceholderKey(service.ID)]
		if placeholder == "" {
			return nil, fmt.Errorf("MCP 服务「%s」缺少占位符", service.Name)
		}
		routes = append(routes, gatekeeper.Connector{ID: service.ID, Placeholder: placeholder,
			Upstream: "http://" + ip + ":" + connectorPort, Token: token, Tools: grantedTools(sbx, service), ToolsRestricted: true})
	}
	return routes, nil
}

func (m *Manager) ensureConnector(ctx context.Context, sandboxID, name, image, network string) error {
	info := m.container(ctx, name)
	if info.exists && (info.image != image || info.spec != specVersion) {
		if _, err := m.docker.run(ctx, 30*time.Second, nil, "rm", "--force", name); err != nil {
			return err
		}
		info = containerInfo{} // the replacement still has to be created and started
		m.mu.Lock()
		delete(m.connectorTokens, name)
		m.mu.Unlock()
	}
	if !info.exists {
		if _, err := m.docker.run(ctx, 30*time.Second, nil, "create", "--name", name,
			"--label", labelKey+"="+sandboxID, "--label", specLabel+"="+specVersion, "--label", roleLabel+"="+connectorRole,
			"--network", network, "--read-only", "--init",
			"--tmpfs", "/run/aiofcage:uid=1000,gid=1000,mode=0700,size=1m",
			"--tmpfs", "/tmp:uid=1000,gid=1000,mode=1777,size=64m",
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
			"--memory", "512m", "--cpus", "1", "--pids-limit", "256", image); err != nil {
			return err
		}
	}
	if !info.running {
		// A fresh start empties the tmpfs, so the connector waits for a new launch file.
		if _, err := m.docker.run(ctx, 30*time.Second, nil, "start", name); err != nil {
			return err
		}
	}
	return nil
}

// connectorToken returns the gatekeeper→connector credential for a container, creating one the
// first time. Tokens live only in memory here, in the gatekeeper and in the connector.
func (m *Manager) connectorToken(name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if token := m.connectorTokens[name]; token != "" {
		return token, nil
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	m.connectorTokens[name] = token
	return token, nil
}

// RuntimeSecrets returns the credentials of a sandbox that exist only in memory: the gatekeeper's
// management token and the gatekeeper→connector tokens. Isolation checks compare what the agent
// can see against them.
func (m *Manager) RuntimeSecrets(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var values []string
	if gate := m.gates[id]; gate != nil && gate.token != "" {
		values = append(values, gate.token)
	}
	prefix := connectorName(id, "")
	for name, token := range m.connectorTokens {
		if strings.HasPrefix(name, prefix) && token != "" {
			values = append(values, token)
		}
	}
	return values
}

func randomToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func (m *Manager) containerIP(ctx context.Context, name, network string) (string, error) {
	out, err := m.docker.run(ctx, 15*time.Second, nil, "container", "inspect", "--format",
		`{{with index .NetworkSettings.Networks "`+network+`"}}{{.IPAddress}}{{end}}`, name)
	ip := strings.TrimSpace(string(out))
	if err != nil || ip == "" {
		return "", fmt.Errorf("无法读取连接器 %s 的地址", name)
	}
	return ip, nil
}

// ErrConnectorNotBuilt means the connector image does not exist yet; it is built the first time a
// sandbox with a connector service starts.
var ErrConnectorNotBuilt = errors.New("连接器镜像还没有构建")

// CheckConnector logs in with the service's secret from a throwaway connector container and runs
// the connector's read-only check call. It returns the server's answer, or its error message
// (e.g. a wrong password) as a failed result; err is reserved for problems running the check.
func (m *Manager) CheckConnector(ctx context.Context, service mcp.Service, secret string) (bool, string, error) {
	spec, ok := mcp.ConnectorFor(service.Kind)
	if !ok || spec.CheckTool == "" {
		return false, "", errors.New("这个服务不支持连接检查")
	}
	build, err := connectorContext()
	if err != nil {
		return false, "", err
	}
	if _, err := m.docker.run(ctx, 15*time.Second, nil, "image", "inspect", build.tag); err != nil {
		// After an update only the small connector layer changes, so rebuilding takes seconds. A
		// first-ever build downloads the database servers and is left to the first sandbox start.
		out, listErr := m.docker.run(ctx, 15*time.Second, nil, "images", "--quiet", "aiofcage/connector")
		if listErr != nil || len(strings.TrimSpace(string(out))) == 0 {
			return false, "", ErrConnectorNotBuilt
		}
		if err := m.ensureImage(ctx, build, nil); err != nil {
			return false, "", err
		}
	}
	command, redact, err := service.Launch(secret)
	if err != nil {
		return false, "", err
	}
	token, err := randomToken()
	if err != nil {
		return false, "", err
	}
	payload, err := json.Marshal(map[string]any{"launch": connector.Launch{Token: token, Command: command, Redact: redact},
		"tool": spec.CheckTool, "arguments": spec.CheckArguments})
	if err != nil {
		return false, "", err
	}
	// Same restrictions as a real connector; the default bridge network reaches the database the
	// same way the connector's egress network does.
	out, err := m.docker.run(ctx, 60*time.Second, payload, "run", "--rm", "--interactive", "--network", "bridge",
		"--label", "io.aiofcage.managed=true", "--read-only", "--tmpfs", "/tmp:uid=1000,gid=1000,mode=1777,size=64m",
		"--user", connectorUser, "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", "256m", "--pids-limit", "128", "--entrypoint", connectorCommand, build.tag, "check")
	if err != nil {
		return false, "", fmt.Errorf("无法运行连接检查：%w", err)
	}
	var result struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return false, "", errors.New("连接检查没有返回结果")
	}
	return result.OK, result.Message, nil
}
