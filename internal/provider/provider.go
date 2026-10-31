// Package provider describes model services that agents in a sandbox can use. A provider holds
// only non-secret metadata; the API key lives in the macOS Keychain under KeyRef.
package provider

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/shaoxier/AIofCage/internal/policy"
	"github.com/shaoxier/AIofCage/internal/session"
)

type Protocol string

const (
	OpenAIResponses Protocol = "openai-responses"
	OpenAIChat      Protocol = "openai-chat"
	Anthropic       Protocol = "anthropic"
)

// Auth selects how the key is sent upstream; the sandbox only ever sees a placeholder in the
// same position.
type Auth string

const (
	AuthBearer  Auth = "bearer"    // Authorization: Bearer <key>
	AuthXAPIKey Auth = "x-api-key" // X-Api-Key: <key>
)

type Provider struct {
	Pricing    *Pricing      `json:"pricing,omitempty"`
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Agent      session.Agent `json:"agent"`
	Protocol   Protocol      `json:"protocol"`
	BaseURL    string        `json:"base_url"`
	Model      string        `json:"model"`
	ImageInput bool          `json:"image_input,omitempty"`
	Auth       Auth          `json:"auth"`
	KeyRef     string        `json:"key_ref"`
	Preset     string        `json:"preset,omitempty"`
	CreatedAt  time.Time     `json:"created_at"`
}

var (
	// modelPattern accepts the model names providers list (gpt-6.1-sol, deepseek-ai/DeepSeek-V3,
	// qwen:72b, org/model+beta, @cf/meta/llama-3-8b-instruct). Configurations quote the name.
	modelPattern  = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9._:/@+-]{0,199}$`)
	keyRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// ValidModel reports whether a model name can be saved for a provider.
func ValidModel(name string) bool { return modelPattern.MatchString(name) }

// KeyRefFor is the Keychain service name used for keys entered in AIofCage.
func KeyRefFor(id string) string { return "aiofcage-provider-" + id }

// Compatible reports which protocols each agent can speak.
func Compatible(agent session.Agent, protocol Protocol) bool {
	switch agent {
	case session.Codex:
		// Codex 0.164 only speaks the Responses API; wire_api = "chat" is rejected at startup.
		return protocol == OpenAIResponses
	case session.ClaudeCode:
		return protocol == Anthropic
	case session.DSH:
		// 固定版本的 llm-pi-ai 支持三种自定义协议。
		return protocol == Anthropic || protocol == OpenAIChat || protocol == OpenAIResponses
	}
	return false
}

// DSHBaseURL is the only endpoint dsh can use without patching its profile YAML.
const DSHBaseURL = "https://api.deepseek.com/anthropic"

func (p *Provider) Normalize() {
	p.Name = strings.TrimSpace(p.Name)
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	p.Model = strings.TrimSpace(p.Model)
	if p.Auth == "" {
		p.Auth = AuthBearer
	}
}

func (p Provider) Validate() error {
	if p.Pricing != nil {
		if err := p.Pricing.Validate(); err != nil {
			return err
		}
	}
	if !policy.ValidID(p.ID) {
		return errors.New("供应商 ID 只能包含字母、数字、点、下划线和连字符")
	}
	if p.Name == "" || len([]rune(p.Name)) > 60 {
		return errors.New("供应商名称须为 1 至 60 个字符")
	}
	if !p.Agent.Valid() {
		return errors.New("请选择供应商所属的 Agent")
	}
	if !Compatible(p.Agent, p.Protocol) {
		return fmt.Errorf("%s 不支持 %s 协议", p.Agent, p.Protocol)
	}
	if _, _, err := p.Endpoint(); err != nil {
		return err
	}
	if !modelPattern.MatchString(p.Model) {
		return errors.New("模型名称格式无效")
	}
	if p.Auth != AuthBearer && p.Auth != AuthXAPIKey {
		return errors.New("认证方式只能是 bearer 或 x-api-key")
	}
	if !keyRefPattern.MatchString(p.KeyRef) {
		return errors.New("钥匙串条目名称格式无效")
	}
	if p.Agent == session.DSH && ((p.Protocol == Anthropic && p.Auth != AuthXAPIKey) || (p.Protocol != Anthropic && p.Auth != AuthBearer)) {
		return errors.New("DeepSeek Harness 的 Anthropic 协议使用 x-api-key，OpenAI 协议使用 bearer")
	}
	return p.KeyRefAllowed()
}

// KeyRefAllowed reports whether this provider may send its key to its endpoint. Items AIofCage
// created for the provider go anywhere the user points them; a Keychain item named by a preset
// (such as the competition key) stays bound to that preset's host, so editing the address
// cannot redirect it elsewhere.
func (p Provider) KeyRefAllowed() error {
	if p.KeyRef == KeyRefFor(p.ID) {
		return nil
	}
	host, _, err := p.Endpoint()
	if err != nil {
		return err
	}
	var hosts []string
	for _, preset := range Presets() {
		if preset.ExistingKeyRef == "" || preset.ExistingKeyRef != p.KeyRef {
			continue
		}
		presetHost, _, err := Provider{BaseURL: preset.BaseURL}.Endpoint()
		if err != nil {
			continue
		}
		if presetHost == host {
			return nil
		}
		hosts = append(hosts, presetHost)
	}
	if len(hosts) == 0 {
		return fmt.Errorf("钥匙串条目 %s 不是 AIofCage 为这个供应商保存的密钥", p.KeyRef)
	}
	return fmt.Errorf("钥匙串条目 %s 只能发往 %s；改用其他接口地址时请填写自己的 API Key", p.KeyRef, strings.Join(hosts, "、"))
}

// Endpoint returns the HTTPS host and base path of BaseURL.
func (p Provider) Endpoint() (string, string, error) {
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Port() != "" && u.Port() != "443") {
		return "", "", errors.New("接口地址须为 https:// 开头的网址，不能带端口、“?”查询参数或“#”片段")
	}
	host := strings.ToLower(u.Hostname())
	if !policy.ValidHost(host, false) {
		return "", "", errors.New("接口地址的域名无效（不支持 IP 地址）")
	}
	base := u.EscapedPath()
	if base == "" {
		base = "/"
	}
	if base != "/" && !policy.ValidPath(base) {
		return "", "", errors.New("接口地址的路径无效")
	}
	if strings.Contains(base, "*") {
		// A "*" segment would turn the provider's rules into wildcards.
		return "", "", errors.New("接口地址的路径里不能有 *")
	}
	return host, strings.TrimSuffix(base, "/"), nil
}

// Rules authorizes exactly the API calls the agent needs for this provider, with the key injected.
func (p Provider) Rules() []policy.Rule {
	host, base, err := p.Endpoint()
	if err != nil {
		return nil
	}
	join := func(elem ...string) string { return path.Join(append([]string{"/", base}, elem...)...) }
	var rules []policy.Rule
	add := func(suffix string, methods []string, pattern string) {
		rules = append(rules, policy.Rule{ID: "p-" + p.ID + "-" + suffix, Effect: policy.Allow, Host: host,
			Methods: methods, Path: pattern, SecretRef: p.ID})
	}
	switch p.Protocol {
	case OpenAIResponses:
		add("responses", []string{"POST"}, join("responses")+"/*")
		add("models", []string{"GET"}, join("models"))
	case OpenAIChat:
		add("chat", []string{"POST"}, join("chat", "completions"))
		add("models", []string{"GET"}, join("models"))
	case Anthropic:
		add("messages", []string{"POST"}, join("v1", "messages")+"/*")
		add("models", []string{"GET"}, join("v1", "models"))
	}
	return rules
}

// Injection returns the header and prefix that carry the key.
func (p Provider) Injection() (string, string) {
	if p.Auth == AuthXAPIKey {
		return "X-Api-Key", ""
	}
	return "Authorization", "Bearer "
}
