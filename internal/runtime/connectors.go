package runtime

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shaoxier/AIofCage/internal/connector"
	"github.com/shaoxier/AIofCage/internal/gatekeeper"
	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/sandbox"
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

// grantedTools is the sandbox's grant for a service. Sandboxes saved before grants existed get
// the service's default tools until a grant is pinned (see pinGrants).
func grantedTools(sbx sandbox.Sandbox, service mcp.Service) []string {
	if tools, ok := sbx.MCPPermissions[service.ID]; ok {
		return append([]string{}, tools...)
	}
	return service.DefaultTools()
}

// prepareRemote lists the tools of remote services whose tools were never listed, and pins the
// default grant of services the sandbox uses without one once their tools are known: tools a
// service adds later must not be granted without the user choosing them. services is updated.
func (m *Manager) prepareRemote(ctx context.Context, sbx sandbox.Sandbox, services map[string]mcp.Service) (sandbox.Sandbox, error) {
	pins := map[string][]string{}
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
		if _, ok := sbx.MCPPermissions[id]; !ok && service.CatalogAt != nil {
			pins[id] = service.DefaultTools()
		}
	}
	if len(pins) == 0 {
		return sbx, nil
	}
	pin := func(s *sandbox.Sandbox) {
		if s.MCPPermissions == nil {
			s.MCPPermissions = map[string][]string{}
		}
		for id, tools := range pins {
			if _, ok := s.MCPPermissions[id]; !ok {
				s.MCPPermissions[id] = append([]string{}, tools...)
			}
		}
	}
	if err := m.store.UpdateSandbox(sbx.ID, func(s *sandbox.Sandbox) error { pin(s); return nil }); err != nil {
		return sbx, err
	}
	sbx.MCPPermissions = cloneMCPPermissions(sbx.MCPPermissions)
	pin(&sbx)
	return sbx, nil
}

// ListTools asks a remote service for its tools with the token the host holds and saves them as
// the service's tool list. Grants are not changed.
func (m *Manager) ListTools(ctx context.Context, service mcp.Service) (mcp.Service, error) {
	upstream, err := service.Upstream()
	if err != nil {
		return service, err
	}
	token, err := m.serviceToken(ctx, service)
	if err != nil {
		return service, err
	}
	listCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tools, err := mcp.ListTools(listCtx, m.http, upstream, token)
	if err != nil {
		return service, err
	}
	now := time.Now().UTC()
	service.Catalog, service.CatalogAt = tools, &now
	return service, m.store.SaveMCPService(service)
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
