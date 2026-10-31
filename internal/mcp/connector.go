package mcp

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/shaoxier/AIofCage/internal/democases"
)

type Kind string

const (
	KindHTTP Kind = "http"
	// KindOAuth is a remote HTTP service whose token comes from an OAuth login on the host.
	KindOAuth    Kind = "oauth"
	KindPostgres Kind = "postgres"
	KindDemo     Kind = "demo"
)

// ConnectorSpec pins the stdio MCP server a connector runs. Packages are installed into the
// connector image at fixed versions, never fetched at run time.
type ConnectorSpec struct {
	Kind    Kind
	Name    string
	Package string
	Version string
	Command string
	// Tools are the tools agents may call; the gatekeeper refuses every other tool.
	Tools []string
	// CheckTool and CheckArguments make the read-only call used by "测试连接".
	CheckTool      string
	CheckArguments map[string]any
	DefaultPort    string
	Schemes        []string
}

var connectorCatalog = []ConnectorSpec{
	{Kind: KindDemo, Name: "合成业务案例", Command: "/usr/local/bin/aiofcage-connector", Tools: democases.ToolNames, CheckTool: "case_overview", CheckArguments: map[string]any{}},
	// 自有只读服务同时校验 SQL 结构和账号有效权限，不能依赖上游的只读描述或事务包装。
	{Kind: KindPostgres, Name: "PostgreSQL", Version: "1.0.0",
		Command: "/opt/aiofcage-postgres/server.mjs", Tools: []string{"query"}, DefaultPort: "5432", Schemes: []string{"postgresql", "postgres"},
		CheckTool: "query", CheckArguments: map[string]any{"sql": "SELECT 1"}},
}

func Connectors() []ConnectorSpec { return append([]ConnectorSpec(nil), connectorCatalog...) }

func ConnectorFor(kind Kind) (ConnectorSpec, bool) {
	for _, spec := range connectorCatalog {
		if spec.Kind == kind {
			return spec, true
		}
	}
	return ConnectorSpec{}, false
}

// GatewayPort is the gatekeeper's MCP entrypoint on the sandbox network.
const GatewayPort = 8890

// GatewayURL is where agents reach a service.
func GatewayURL(id string) string { return fmt.Sprintf("http://gatekeeper:%d/mcp/%s", GatewayPort, id) }

// LocalHost is how a connector inside the VM reaches services listening on the Mac's loopback.
const LocalHost = "host.lima.internal"

func (s Service) IsConnector() bool {
	_, ok := ConnectorFor(s.Kind)
	return ok
}

func (s Service) IsOAuth() bool { return s.Kind == KindOAuth }

// AgentURL is the address written into agent configurations: always the gatekeeper's MCP
// entrypoint, never the service itself.
func (s Service) AgentURL() string { return GatewayURL(s.ID) }

// Tools returns the tools a service offers: a connector's fixed catalog, or the tools a remote
// service last listed.
func (s Service) Tools() []string {
	if spec, ok := ConnectorFor(s.Kind); ok && s.IsConnector() {
		return append([]string(nil), spec.Tools...)
	}
	tools := make([]string, 0, len(s.Catalog))
	for _, tool := range s.Catalog {
		tools = append(tools, tool.Name)
	}
	return tools
}

// DefaultTools are granted when a sandbox enables a service without choosing tools: every tool
// of a connector, whose catalog AIofCage fixes, but only the tools a remote service marks as
// read-only. Tools a remote service adds later are never granted by default.
func (s Service) DefaultTools() []string {
	if s.IsConnector() {
		return s.Tools()
	}
	tools := []string{}
	for _, tool := range s.Catalog {
		if tool.ReadOnly {
			tools = append(tools, tool.Name)
		}
	}
	return tools
}

// ValidateTools ensures a grant only names tools the service offers. An empty grant is valid and
// means that the service is enabled but no tool may be called.
func ValidateTools(s Service, tools []string) error {
	allowed := s.Tools()
	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if seen[tool] || !slices.Contains(allowed, tool) {
			return fmt.Errorf("MCP 服务「%s」没有工具「%s」，请先刷新它的工具列表", s.Name, tool)
		}
		seen[tool] = true
	}
	return nil
}

var (
	dbHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	// Parameters that name files, other hosts or options are refused.
	postgresParams = map[string]bool{"sslmode": true, "application_name": true, "connect_timeout": true}
	sslModes       = map[string]bool{"disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true}
)

// SplitDatabaseURL validates a connection URL and returns it without the password, plus the
// password when one was pasted in, so that it can go to the Keychain instead.
func SplitDatabaseURL(kind Kind, raw string) (string, string, error) {
	spec, ok := ConnectorFor(kind)
	if !ok || spec.DefaultPort == "" {
		return "", "", errors.New("不支持的数据库类型")
	}
	example := spec.Schemes[0] + "://用户名@主机:" + spec.DefaultPort + "/数据库"
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !slices.Contains(spec.Schemes, u.Scheme) || u.Opaque != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("连接串格式应为 %s", example)
	}
	if u.User == nil || u.User.Username() == "" {
		return "", "", fmt.Errorf("连接串里缺少用户名，格式应为 %s", example)
	}
	host := strings.ToLower(u.Hostname())
	if _, ipErr := netip.ParseAddr(host); ipErr != nil && !dbHostPattern.MatchString(host) {
		return "", "", errors.New("连接串里的主机名无效")
	}
	if port := u.Port(); port != "" {
		if value, err := strconv.Atoi(port); err != nil || value < 1 || value > 65535 {
			return "", "", errors.New("连接串里的端口无效")
		}
	}
	database := strings.TrimPrefix(u.Path, "/")
	if database == "" || strings.Contains(database, "/") || u.RawPath != "" {
		return "", "", fmt.Errorf("连接串里缺少数据库名，格式应为 %s", example)
	}
	query := u.Query()
	for key, values := range query {
		if !postgresParams[key] || len(values) != 1 {
			return "", "", fmt.Errorf("不支持连接参数 %s（只支持 sslmode、application_name、connect_timeout）", key)
		}
	}
	if mode := query.Get("sslmode"); mode != "" && !sslModes[mode] {
		return "", "", errors.New("sslmode 的取值无效")
	}
	password, _ := u.User.Password()
	u.User = url.User(u.User.Username())
	u.Host = strings.ToLower(u.Host)
	return u.String(), password, nil
}

// Address is where the connector reaches the database: localhost means the Mac, which the VM
// reaches as LocalHost.
func (s Service) Address() (string, error) {
	spec, ok := ConnectorFor(s.Kind)
	if !ok || spec.DefaultPort == "" || !s.IsConnector() {
		return "", errors.New("不是连接器服务")
	}
	u, err := url.Parse(s.URL)
	if err != nil {
		return "", err
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = spec.DefaultPort
	}
	if ip, err := netip.ParseAddr(host); host == "localhost" || (err == nil && ip.IsLoopback()) {
		host = LocalHost
	}
	return net.JoinHostPort(host, port), nil
}

// Launch returns the command line that starts the service's MCP server with its secret, and the
// strings to redact from that server's logs. It is only ever sent to the connector container.
func (s Service) Launch(secret string) ([]string, []string, error) {
	if s.IsBuiltin() {
		if err := s.Validate(); err != nil {
			return nil, nil, err
		}
		return []string{"/usr/local/bin/aiofcage-connector", "demo-stdio", strings.TrimPrefix(s.URL, "builtin://")}, nil, nil
	}
	address, err := s.Address()
	if err != nil {
		return nil, nil, err
	}
	spec, _ := ConnectorFor(s.Kind)
	u, err := url.Parse(s.URL)
	if err != nil {
		return nil, nil, err
	}
	u.Host = address
	u.User = url.UserPassword(u.User.Username(), secret)
	escaped := strings.TrimPrefix(url.UserPassword("x", secret).String(), "x:")
	redact := []string{secret}
	if escaped != secret {
		redact = append(redact, escaped)
	}
	return []string{spec.Command, u.String()}, redact, nil
}
