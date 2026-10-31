package provider

import (
	"strings"
	"testing"

	"github.com/shaoxier/AIofCage/internal/policy"
	"github.com/shaoxier/AIofCage/internal/session"
)

func xapi() Provider {
	p := Provider{ID: "xapi", Name: "七牛 xapi", Agent: session.Codex, Protocol: OpenAIResponses,
		BaseURL: "https://xapi.qiniu.io/v1/", Model: "gpt-6.1-sol", KeyRef: "sbx-builderday-key"}
	p.Normalize()
	return p
}

func TestPresetKeyRefIsBoundToPresetHost(t *testing.T) {
	if err := xapi().KeyRefAllowed(); err != nil {
		t.Fatalf("preset key on the preset host: %v", err)
	}
	moved := xapi()
	moved.BaseURL = "https://collector.example/v1"
	if err := moved.KeyRefAllowed(); err == nil || !strings.Contains(err.Error(), "xapi.qiniu.io") {
		t.Fatalf("preset key allowed on another host: %v", err)
	}
	if err := moved.Validate(); err == nil {
		t.Fatal("Validate accepted the preset key on another host")
	}
	own := moved
	own.KeyRef = KeyRefFor(own.ID)
	if err := own.KeyRefAllowed(); err != nil {
		t.Fatalf("a key AIofCage saved for the provider must go to any host: %v", err)
	}
	foreign := xapi()
	foreign.KeyRef = "github.com"
	if err := foreign.KeyRefAllowed(); err == nil {
		t.Fatal("arbitrary Keychain item allowed")
	}
}

func TestValidateAndEndpoint(t *testing.T) {
	p := xapi()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	host, base, err := p.Endpoint()
	if host != "xapi.qiniu.io" || base != "/v1" || err != nil {
		t.Fatalf("endpoint %q %q %v", host, base, err)
	}
	bad := map[string]func(*Provider){
		"http":          func(p *Provider) { p.BaseURL = "http://xapi.qiniu.io/v1" },
		"ip":            func(p *Provider) { p.BaseURL = "https://1.2.3.4/v1" },
		"port":          func(p *Provider) { p.BaseURL = "https://xapi.qiniu.io:8443/v1" },
		"query":         func(p *Provider) { p.BaseURL = "https://xapi.qiniu.io/v1?x=1" },
		"userinfo":      func(p *Provider) { p.BaseURL = "https://u:p@xapi.qiniu.io/v1" },
		"codex chat":    func(p *Provider) { p.Protocol = OpenAIChat },
		"claude openai": func(p *Provider) { p.Agent = session.ClaudeCode },
		"empty model":   func(p *Provider) { p.Model = "" },
		"bad keyref":    func(p *Provider) { p.KeyRef = "has space" },
		"bad auth":      func(p *Provider) { p.Auth = "basic" },
		"no name":       func(p *Provider) { p.Name = "" },
		"unknown agent": func(p *Provider) { p.Agent = "cursor" },
		"encoded path":  func(p *Provider) { p.BaseURL = "https://xapi.qiniu.io/v%31" },
		// A "*" segment would make the injecting rules wildcards.
		"star path":    func(p *Provider) { p.BaseURL = "https://xapi.qiniu.io/*" },
		"star segment": func(p *Provider) { p.BaseURL = "https://xapi.qiniu.io/v1/*/x" },
	}
	for name, mutate := range bad {
		candidate := xapi()
		mutate(&candidate)
		candidate.Normalize()
		if candidate.Validate() == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRulesAreValidAndMinimal(t *testing.T) {
	cases := []struct {
		provider Provider
		allowed  []policy.Request
		denied   []policy.Request
	}{
		{xapi(),
			[]policy.Request{{Host: "xapi.qiniu.io", Method: "POST", Path: "/v1/responses"}, {Host: "xapi.qiniu.io", Method: "POST", Path: "/v1/responses/compact"}, {Host: "xapi.qiniu.io", Method: "GET", Path: "/v1/models"}},
			[]policy.Request{{Host: "xapi.qiniu.io", Method: "POST", Path: "/v1/files"}, {Host: "xapi.qiniu.io", Method: "GET", Path: "/v1/responses"}, {Host: "xapi.qiniu.io", Method: "POST", Path: "/v1/chat/completions"}}},
		{Provider{ID: "ds", Name: "DeepSeek", Agent: session.ClaudeCode, Protocol: Anthropic, BaseURL: "https://api.deepseek.com/anthropic", Model: "deepseek-chat", KeyRef: "k"},
			[]policy.Request{{Host: "api.deepseek.com", Method: "POST", Path: "/anthropic/v1/messages"}, {Host: "api.deepseek.com", Method: "POST", Path: "/anthropic/v1/messages/count_tokens"}},
			[]policy.Request{{Host: "api.deepseek.com", Method: "POST", Path: "/v1/messages"}, {Host: "api.deepseek.com", Method: "POST", Path: "/anthropic/v1/files"}}},
	}
	for _, c := range cases {
		c.provider.Normalize()
		snapshot := policy.Snapshot{Version: 1, Rules: c.provider.Rules()}
		if err := snapshot.Validate(); err != nil {
			t.Fatalf("%s rules invalid: %v", c.provider.ID, err)
		}
		for _, r := range c.allowed {
			if d := snapshot.Evaluate(r); !d.Allowed || d.SecretRef != c.provider.ID {
				t.Errorf("%s should allow %+v: %+v", c.provider.ID, r, d)
			}
		}
		for _, r := range c.denied {
			if d := snapshot.Evaluate(r); d.Allowed {
				t.Errorf("%s should deny %+v", c.provider.ID, r)
			}
		}
	}
}

func TestInjection(t *testing.T) {
	p := xapi()
	if h, prefix := p.Injection(); h != "Authorization" || prefix != "Bearer " {
		t.Fatal(h, prefix)
	}
	p.Auth = AuthXAPIKey
	if h, prefix := p.Injection(); h != "X-Api-Key" || prefix != "" {
		t.Fatal(h, prefix)
	}
}

func TestPresetsAreValid(t *testing.T) {
	seen := map[string]bool{}
	for _, preset := range Presets() {
		if seen[preset.ID] {
			t.Errorf("duplicate preset %s", preset.ID)
		}
		seen[preset.ID] = true
		if preset.BaseURL == "" { // custom presets are filled in by the user
			continue
		}
		p := Provider{ID: "p", Name: preset.Name, Agent: preset.Agent, Protocol: preset.Protocol, BaseURL: preset.BaseURL,
			Model: preset.Model, Auth: preset.Auth, KeyRef: KeyRefFor("p")}
		p.Normalize()
		if err := p.Validate(); err != nil {
			t.Errorf("preset %s invalid: %v", preset.ID, err)
		}
	}
}
