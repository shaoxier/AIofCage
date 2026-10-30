// Package policy validates and evaluates network authorization rules. Deny always wins over allow,
// and anything not explicitly allowed is denied.
package policy

import (
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"strings"
)

// Rule authorizes or denies HTTPS requests. Host is an exact lowercase name; deny rules may also use
// a "*.example.com" suffix wildcard. Path is exact, a prefix ending in "/*", or empty for any path;
// a segment that is just "*" matches any one non-empty segment (e.g. "/*/*/info/refs").
type Rule struct {
	ID        string   `json:"id"`
	Effect    string   `json:"effect"`
	Host      string   `json:"host"`
	Port      uint16   `json:"port,omitempty"`
	Methods   []string `json:"methods,omitempty"`
	Path      string   `json:"path,omitempty"`
	SecretRef string   `json:"secret_ref,omitempty"`
	// Anonymous allow rules refuse requests that carry their own credentials (an Authorization,
	// API key or Cookie header): the source is opened for anonymous downloads only.
	Anonymous bool `json:"anonymous,omitempty"`
}

type Snapshot struct {
	Version   uint64 `json:"version"`
	SessionID string `json:"session_id"`
	Rules     []Rule `json:"rules"`
}

type Request struct {
	Host   string
	Port   uint16
	Method string
	Path   string
}

type Decision struct {
	Allowed    bool
	ReasonCode string
	RuleID     string
	SecretRef  string
	Anonymous  bool
	// Matches lists every allow rule matching the request, in order. Several providers may share
	// a host and path (e.g. two agents on the same endpoint); the gatekeeper then picks the one
	// whose placeholder the request carries.
	Matches []Match
}

type Match struct {
	RuleID    string
	SecretRef string
	Anonymous bool
}

const (
	Allow = "allow"
	Deny  = "deny"

	// Only HTTPS through the MITM gatekeeper is supported in the first version.
	DefaultPort = 443
	maxRules    = 256
)

var (
	idPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	methods      = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true}
)

// DenyUnconfigured is returned before any snapshot has been activated.
func DenyUnconfigured() Decision {
	return Decision{Allowed: false, ReasonCode: "policy_not_configured"}
}

// ValidID reports whether value can be used as a rule, secret, or session identifier.
func ValidID(value string) bool { return idPattern.MatchString(value) }

// ValidHost accepts lowercase DNS names with at least one dot. IP literals are rejected so that
// authorization always refers to names the gatekeeper resolves and checks itself.
func ValidHost(host string, allowWildcard bool) bool {
	if allowWildcard && strings.HasPrefix(host, "*.") {
		host = host[2:]
	}
	if host == "" || len(host) > 253 || host != strings.ToLower(host) || !strings.Contains(host, ".") {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !labelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

// ValidPath accepts the request paths the gatekeeper is willing to evaluate: absolute, already
// clean, and without encoded or ambiguous characters.
// A single trailing slash is allowed (e.g. MCP endpoints such as /mcp/).
func ValidPath(value string) bool {
	trimmed := value
	if len(value) > 1 {
		trimmed = strings.TrimSuffix(value, "/")
		if strings.HasSuffix(trimmed, "/") {
			return false // "//" or a doubled trailing slash
		}
	}
	return strings.HasPrefix(value, "/") && path.Clean(trimmed) == trimmed && !strings.ContainsAny(value, "%?#\\")
}

func (s *Snapshot) Validate() error {
	if s.Version == 0 {
		return errors.New("policy version 必须大于 0")
	}
	if s.SessionID != "" && !ValidID(s.SessionID) {
		return errors.New("session_id 格式无效")
	}
	if len(s.Rules) > maxRules {
		return fmt.Errorf("规则不能超过 %d 条", maxRules)
	}
	seen := map[string]bool{}
	for i, rule := range s.Rules {
		if err := rule.validate(); err != nil {
			return fmt.Errorf("第 %d 条规则：%w", i+1, err)
		}
		if seen[rule.ID] {
			return fmt.Errorf("规则 ID 重复：%s", rule.ID)
		}
		seen[rule.ID] = true
	}
	return nil
}

func (r Rule) validate() error {
	if !ValidID(r.ID) {
		return errors.New("规则 ID 格式无效")
	}
	if r.Effect != Allow && r.Effect != Deny {
		return errors.New("effect 只能是 allow 或 deny")
	}
	if !ValidHost(r.Host, r.Effect == Deny) {
		return errors.New("host 须为小写域名；只有 deny 规则可用 *. 通配")
	}
	if r.Port != 0 && r.Port != DefaultPort {
		return errors.New("第一版只支持 443 端口")
	}
	if r.Effect == Allow && len(r.Methods) == 0 {
		return errors.New("allow 规则必须列出 HTTP 方法")
	}
	for _, method := range r.Methods {
		if !methods[method] {
			return fmt.Errorf("不支持的 HTTP 方法：%s", method)
		}
	}
	if r.Path != "" && r.Path != "/*" && !validPattern(strings.TrimSuffix(r.Path, "/*")) {
		return errors.New("path 须为规范化的绝对路径，前缀匹配以 /* 结尾，单段通配只能写成 /*/")
	}
	if r.SecretRef != "" && (r.Effect != Allow || !ValidID(r.SecretRef)) {
		return errors.New("secret_ref 只能用于 allow 规则且须为有效 ID")
	}
	if r.Anonymous && (r.Effect != Allow || r.SecretRef != "") {
		return errors.New("anonymous 只能用于不注入密钥的 allow 规则")
	}
	return nil
}

// validPattern accepts a clean absolute path whose segments are literal or exactly "*".
func validPattern(base string) bool {
	if !strings.HasPrefix(base, "/") {
		return false
	}
	segments := strings.Split(base[1:], "/")
	for i, segment := range segments {
		switch {
		case segment == "*":
			segments[i] = "x"
		case strings.Contains(segment, "*"):
			return false
		}
	}
	return ValidPath("/" + strings.Join(segments, "/"))
}

// Evaluate applies deny rules first, then the first matching allow rule.
func (s *Snapshot) Evaluate(request Request) Decision {
	request = normalize(request)
	for _, rule := range s.Rules {
		if rule.Effect == Deny && rule.matches(request) {
			return Decision{Allowed: false, ReasonCode: "rule_denied", RuleID: rule.ID}
		}
	}
	var decision Decision
	for _, rule := range s.Rules {
		if rule.Effect == Allow && rule.matches(request) {
			if !decision.Allowed {
				decision = Decision{Allowed: true, ReasonCode: "allowed", RuleID: rule.ID, SecretRef: rule.SecretRef, Anonymous: rule.Anonymous}
			}
			decision.Matches = append(decision.Matches, Match{RuleID: rule.ID, SecretRef: rule.SecretRef, Anonymous: rule.Anonymous})
		}
	}
	if decision.Allowed {
		return decision
	}
	return Decision{Allowed: false, ReasonCode: "default_deny"}
}

// AllowsConnect decides whether a tunnel to host:port may be opened at all. Individual requests
// inside the tunnel are still evaluated with Evaluate after TLS interception.
func (s *Snapshot) AllowsConnect(host string, port uint16) Decision {
	request := normalize(Request{Host: host, Port: port})
	for _, rule := range s.Rules {
		if rule.Effect == Deny && len(rule.Methods) == 0 && (rule.Path == "" || rule.Path == "/*") &&
			rule.hostMatches(request.Host) && rule.port() == request.Port {
			return Decision{Allowed: false, ReasonCode: "rule_denied", RuleID: rule.ID}
		}
	}
	for _, rule := range s.Rules {
		if rule.Effect == Allow && rule.hostMatches(request.Host) && rule.port() == request.Port {
			return Decision{Allowed: true, ReasonCode: "allowed", RuleID: rule.ID}
		}
	}
	return Decision{Allowed: false, ReasonCode: "default_deny"}
}

// SecretHosts lists hosts that have at least one rule injecting a secret, with the secret IDs.
func (s *Snapshot) SecretHosts() map[string][]string {
	result := map[string][]string{}
	for _, rule := range s.Rules {
		if rule.Effect == Allow && rule.SecretRef != "" {
			result[rule.Host] = appendUnique(result[rule.Host], rule.SecretRef)
		}
	}
	return result
}

func normalize(request Request) Request {
	request.Host = strings.ToLower(strings.TrimSuffix(request.Host, "."))
	if request.Port == 0 {
		request.Port = DefaultPort
	}
	return request
}

func (r Rule) port() uint16 {
	if r.Port == 0 {
		return DefaultPort
	}
	return r.Port
}

func (r Rule) hostMatches(host string) bool {
	if strings.HasPrefix(r.Host, "*.") {
		return strings.HasSuffix(host, r.Host[1:])
	}
	return host == r.Host
}

func (r Rule) matches(request Request) bool {
	if !r.hostMatches(request.Host) || r.port() != request.Port {
		return false
	}
	if len(r.Methods) > 0 && !contains(r.Methods, request.Method) {
		return false
	}
	return pathMatches(r.Path, request.Path)
}

func pathMatches(pattern, value string) bool {
	if pattern == "" || pattern == "/*" {
		return true
	}
	base, prefix := strings.CutSuffix(pattern, "/*")
	if !strings.Contains(base, "*") {
		if prefix {
			return value == base || strings.HasPrefix(value, base+"/")
		}
		return value == pattern
	}
	want, got := strings.Split(base, "/"), strings.Split(value, "/")
	if len(got) < len(want) || (!prefix && len(got) != len(want)) {
		return false
	}
	for i, segment := range want {
		if segment == "*" && got[i] == "" || segment != "*" && segment != got[i] {
			return false
		}
	}
	return true
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func appendUnique(values []string, value string) []string {
	if contains(values, value) {
		return values
	}
	return append(values, value)
}
