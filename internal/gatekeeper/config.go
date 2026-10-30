package gatekeeper

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/shaoxier/AIofCage/internal/policy"
	"github.com/shaoxier/AIofCage/internal/provider"
)

// Config is delivered by the desktop app through PUT /config. It is the only place real secret
// values enter the gatekeeper, and it is never serialized back out.
type Config struct {
	Policy     policy.Snapshot `json:"policy"`
	Secrets    []Secret        `json:"secrets"`
	Limits     Limits          `json:"limits"`
	Connectors []Connector     `json:"connectors,omitempty"`
}

// Connector routes the MCP entrypoint to a service holding a secret the sandbox never sees. The
// sandbox authenticates with Placeholder. For a trusted connector container (e.g. one holding a
// database password) the gatekeeper forwards to Upstream with Token, a credential only it and the
// connector know. For a Remote service, Upstream is the service's exact https:// endpoint, reached
// through the proxy's public-only dialer with verified TLS, and Token is the service's own bearer
// token (empty for a service that needs none).
type Connector struct {
	ID          string `json:"id"`
	Placeholder string `json:"placeholder"`
	Upstream    string `json:"upstream"`
	Token       string `json:"token"`
	// Tools lists the tools the agent may call. When ToolsRestricted is true, an empty
	// list denies every tool; this keeps an explicit empty grant distinct from legacy
	// connectors that did not carry a tool policy. Remote services are always restricted.
	Tools           []string `json:"tools,omitempty"`
	ToolsRestricted bool     `json:"tools_restricted,omitempty"`
	Remote          bool     `json:"remote,omitempty"`
}

// Secret maps a placeholder visible inside the sandbox to the real credential for one provider.
type Secret struct {
	Pricing     *provider.Pricing `json:"pricing,omitempty"`
	Model       string            `json:"model,omitempty"`
	Agent       string            `json:"agent,omitempty"`
	Name        string            `json:"name,omitempty"`
	ID          string            `json:"id"`
	Placeholder string            `json:"placeholder"`
	Header      string            `json:"header"`
	Prefix      string            `json:"prefix"`
	Value       string            `json:"value"`
}

// Limits are enforced per sandbox. Baselines let the app carry usage across gatekeeper restarts
// so that restarting a sandbox does not reset its budget.
type Limits struct {
	MaxRequests      uint64 `json:"max_requests,omitempty"`
	MaxTokens        uint64 `json:"max_tokens,omitempty"`
	BaselineRequests uint64 `json:"baseline_requests,omitempty"`
	BaselineTokens   uint64 `json:"baseline_tokens,omitempty"`
}

const (
	maxSecrets     = 32
	maxSecretBytes = 4096
	maxConnectors  = 16
	maxGrantTools  = 512
)

var (
	placeholderPattern = regexp.MustCompile(`^cage-[A-Za-z0-9_-]{16,128}$`)
	connectorIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	// Credential headers the gatekeeper understands. Anything else is not treated as a credential.
	credentialHeaders = []string{"Authorization", "X-Api-Key"}
)

// compiled is the immutable, validated form used on the request path.
type compiled struct {
	config      Config
	secrets     map[string]Secret
	secretHosts map[string][]Secret
	connectors  map[string]Connector
}

func compile(config Config) (*compiled, error) {
	if err := config.Policy.Validate(); err != nil {
		return nil, err
	}
	if len(config.Secrets) > maxSecrets {
		return nil, fmt.Errorf("密钥不能超过 %d 个", maxSecrets)
	}
	result := &compiled{config: config, secrets: map[string]Secret{}, secretHosts: map[string][]Secret{},
		connectors: map[string]Connector{}}
	placeholders := map[string]bool{}
	for i, secret := range config.Secrets {
		secret.Header = http.CanonicalHeaderKey(secret.Header)
		config.Secrets[i] = secret
		// Messages only name the secret ID; values and placeholders never appear in errors.
		if !policy.ValidID(secret.ID) {
			return nil, fmt.Errorf("第 %d 个密钥的 ID 无效", i+1)
		}
		if _, ok := result.secrets[secret.ID]; ok {
			return nil, fmt.Errorf("密钥 ID 重复：%s", secret.ID)
		}
		if !placeholderPattern.MatchString(secret.Placeholder) || placeholders[secret.Placeholder] {
			return nil, fmt.Errorf("密钥 %s 的占位符无效或重复", secret.ID)
		}
		if !isCredentialHeader(secret.Header) {
			return nil, fmt.Errorf("密钥 %s 的注入头只能是 Authorization 或 X-Api-Key", secret.ID)
		}
		if secret.Prefix != "" && secret.Prefix != "Bearer " {
			return nil, fmt.Errorf("密钥 %s 的前缀只能为空或 \"Bearer \"", secret.ID)
		}
		if secret.Value == "" || len(secret.Value) > maxSecretBytes || strings.ContainsAny(secret.Value, " \t\r\n") ||
			secret.Value == secret.Placeholder || strings.HasPrefix(secret.Value, "cage-") {
			return nil, fmt.Errorf("密钥 %s 的值无效", secret.ID)
		}
		placeholders[secret.Placeholder] = true
		result.secrets[secret.ID] = secret
	}
	for host, refs := range config.Policy.SecretHosts() {
		for _, ref := range refs {
			secret, ok := result.secrets[ref]
			if !ok {
				return nil, fmt.Errorf("规则引用了不存在的密钥：%s", ref)
			}
			result.secretHosts[host] = append(result.secretHosts[host], secret)
		}
	}
	if len(config.Connectors) > maxConnectors {
		return nil, fmt.Errorf("连接器不能超过 %d 个", maxConnectors)
	}
	for _, connector := range config.Connectors {
		if !connectorIDPattern.MatchString(connector.ID) {
			return nil, errors.New("连接器 ID 无效")
		}
		if _, ok := result.connectors[connector.ID]; ok {
			return nil, fmt.Errorf("连接器 ID 重复：%s", connector.ID)
		}
		if !placeholderPattern.MatchString(connector.Placeholder) || placeholders[connector.Placeholder] {
			return nil, fmt.Errorf("连接器 %s 的占位符无效或重复", connector.ID)
		}
		if connector.Remote {
			if err := validRemote(connector.Upstream); err != nil {
				return nil, fmt.Errorf("连接器 %s：%w", connector.ID, err)
			}
			if len(connector.Token) > maxSecretBytes || strings.ContainsAny(connector.Token, " \t\r\n") ||
				strings.HasPrefix(connector.Token, "cage-") {
				return nil, fmt.Errorf("连接器 %s 的令牌无效", connector.ID)
			}
			connector.ToolsRestricted = true
		} else {
			if err := validUpstream(connector.Upstream); err != nil {
				return nil, fmt.Errorf("连接器 %s：%w", connector.ID, err)
			}
			if len(connector.Token) < 32 || len(connector.Token) > maxSecretBytes || strings.ContainsAny(connector.Token, " \t\r\n") {
				return nil, fmt.Errorf("连接器 %s 的令牌无效", connector.ID)
			}
		}
		if len(connector.Tools) > maxGrantTools {
			return nil, fmt.Errorf("连接器 %s 的工具不能超过 %d 个", connector.ID, maxGrantTools)
		}
		for _, tool := range connector.Tools {
			if tool == "" || len(tool) > 128 || !utf8.ValidString(tool) || strings.ContainsFunc(tool, unicode.IsControl) {
				return nil, fmt.Errorf("连接器 %s 的工具名无效", connector.ID)
			}
		}
		placeholders[connector.Placeholder] = true
		result.connectors[connector.ID] = connector
	}
	if config.Limits.BaselineRequests > 1<<40 || config.Limits.BaselineTokens > 1<<50 {
		return nil, errors.New("用量基线超出范围")
	}
	result.config = config
	return result, nil
}

// validUpstream accepts only http://<private IPv4>:<port>: connectors sit on the sandbox's own
// Docker network, never on the internet.
func validUpstream(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("上游地址须为 http://内网IP:端口")
	}
	address, err := netip.ParseAddrPort(u.Host)
	if err != nil || !address.Addr().Is4() || !address.Addr().IsPrivate() || address.Port() == 0 {
		return errors.New("上游地址须为 http://内网IP:端口")
	}
	return nil
}

// validRemote accepts https://<domain>[:443]/<clean path>: a name the gatekeeper resolves and
// checks itself, never an IP address, a query string or embedded credentials.
func validRemote(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || (u.Port() != "" && u.Port() != "443") || !policy.ValidHost(u.Hostname(), false) {
		return errors.New("远程地址须为 https://域名/路径")
	}
	if p := u.EscapedPath(); p == "" || !policy.ValidPath(p) {
		return errors.New("远程地址的路径无效")
	}
	return nil
}

func isCredentialHeader(name string) bool {
	for _, header := range credentialHeaders {
		if header == name {
			return true
		}
	}
	return false
}
