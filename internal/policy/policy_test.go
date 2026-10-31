package policy

import "testing"

func sample() Snapshot {
	return Snapshot{Version: 1, SessionID: "sbx-1", Rules: []Rule{
		{ID: "model", Effect: Allow, Host: "xapi.qiniu.io", Methods: []string{"POST"}, Path: "/v1/responses", SecretRef: "provider-a"},
		{ID: "npm", Effect: Allow, Host: "registry.npmjs.org", Methods: []string{"GET", "HEAD"}, Path: "/*"},
		{ID: "files", Effect: Deny, Host: "xapi.qiniu.io", Path: "/v1/files/*"},
		{ID: "gh-raw", Effect: Deny, Host: "*.githubusercontent.com"},
		{ID: "gh-read", Effect: Allow, Host: "api.github.com", Methods: []string{"GET"}, Path: "/repos/*"},
	}}
}

func TestValidateAcceptsSample(t *testing.T) {
	s := sample()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]Rule{
		"ip host":             {ID: "r", Effect: Allow, Host: "1.2.3.4", Methods: []string{"GET"}},
		"uppercase host":      {ID: "r", Effect: Allow, Host: "Example.com", Methods: []string{"GET"}},
		"single label":        {ID: "r", Effect: Allow, Host: "localhost", Methods: []string{"GET"}},
		"wildcard allow":      {ID: "r", Effect: Allow, Host: "*.example.com", Methods: []string{"GET"}},
		"allow without verbs": {ID: "r", Effect: Allow, Host: "example.com"},
		"unknown verb":        {ID: "r", Effect: Allow, Host: "example.com", Methods: []string{"TRACE"}},
		"non-443 port":        {ID: "r", Effect: Allow, Host: "example.com", Port: 8443, Methods: []string{"GET"}},
		"dirty path":          {ID: "r", Effect: Allow, Host: "example.com", Methods: []string{"GET"}, Path: "/a/../b"},
		"encoded path":        {ID: "r", Effect: Allow, Host: "example.com", Methods: []string{"GET"}, Path: "/a%2fb"},
		"partial wildcard":    {ID: "r", Effect: Allow, Host: "example.com", Methods: []string{"GET"}, Path: "/a/b*/c"},
		"anonymous deny":      {ID: "r", Effect: Deny, Host: "example.com", Path: "/a", Anonymous: true},
		"anonymous secret":    {ID: "r", Effect: Allow, Host: "example.com", Methods: []string{"GET"}, SecretRef: "s", Anonymous: true},
		"secret on deny":      {ID: "r", Effect: Deny, Host: "example.com", SecretRef: "s"},
		"wildcard secret":     {ID: "r", Effect: Allow, Host: "example.com", Methods: []string{"POST"}, Path: "/*/responses/*", SecretRef: "s"},
		"bad effect":          {ID: "r", Effect: "maybe", Host: "example.com"},
		"bad id":              {ID: "has space", Effect: Deny, Host: "example.com"},
	}
	for name, rule := range cases {
		s := Snapshot{Version: 1, Rules: []Rule{rule}}
		if err := s.Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
	duplicate := Snapshot{Version: 1, Rules: []Rule{
		{ID: "r", Effect: Deny, Host: "a.example"}, {ID: "r", Effect: Deny, Host: "b.example"}}}
	if duplicate.Validate() == nil {
		t.Error("duplicate IDs must be rejected")
	}
	if (&Snapshot{}).Validate() == nil {
		t.Error("version 0 must be rejected")
	}
}

func TestEvaluate(t *testing.T) {
	s := sample()
	cases := []struct {
		name    string
		request Request
		allowed bool
		reason  string
		rule    string
		secret  string
	}{
		{"model call", Request{"xapi.qiniu.io", 443, "POST", "/v1/responses"}, true, "allowed", "model", "provider-a"},
		{"wrong method", Request{"xapi.qiniu.io", 443, "GET", "/v1/responses"}, false, "default_deny", "", ""},
		{"other path", Request{"xapi.qiniu.io", 443, "POST", "/v1/chat/completions"}, false, "default_deny", "", ""},
		{"deny wins", Request{"xapi.qiniu.io", 443, "POST", "/v1/files/abc"}, false, "rule_denied", "files", ""},
		{"deny prefix root", Request{"xapi.qiniu.io", 443, "GET", "/v1/files"}, false, "rule_denied", "files", ""},
		{"prefix allow", Request{"registry.npmjs.org", 0, "GET", "/@openai/codex"}, true, "allowed", "npm", ""},
		{"prefix method", Request{"registry.npmjs.org", 443, "PUT", "/pkg"}, false, "default_deny", "", ""},
		{"case and dot", Request{"API.GITHUB.COM.", 443, "GET", "/repos/a/b"}, true, "allowed", "gh-read", ""},
		{"prefix boundary", Request{"api.github.com", 443, "GET", "/reposx"}, false, "default_deny", "", ""},
		{"unknown host", Request{"example.com", 443, "GET", "/"}, false, "default_deny", "", ""},
		{"other port", Request{"xapi.qiniu.io", 8443, "POST", "/v1/responses"}, false, "default_deny", "", ""},
	}
	for _, c := range cases {
		d := s.Evaluate(c.request)
		if d.Allowed != c.allowed || d.ReasonCode != c.reason || d.RuleID != c.rule || d.SecretRef != c.secret {
			t.Errorf("%s: got %+v", c.name, d)
		}
	}
}

func TestAllowsConnect(t *testing.T) {
	s := sample()
	if d := s.AllowsConnect("xapi.qiniu.io", 443); !d.Allowed {
		t.Errorf("model host tunnel: %+v", d)
	}
	if d := s.AllowsConnect("raw.githubusercontent.com", 443); d.Allowed || d.RuleID != "gh-raw" {
		t.Errorf("host-wide deny must block tunnel: %+v", d)
	}
	if d := s.AllowsConnect("example.com", 443); d.Allowed {
		t.Errorf("unknown host tunnel: %+v", d)
	}
	if d := s.AllowsConnect("xapi.qiniu.io", 80); d.Allowed {
		t.Errorf("non-443 tunnel: %+v", d)
	}
}

func TestSecretHosts(t *testing.T) {
	s := sample()
	hosts := s.SecretHosts()
	if len(hosts) != 1 || len(hosts["xapi.qiniu.io"]) != 1 || hosts["xapi.qiniu.io"][0] != "provider-a" {
		t.Errorf("unexpected secret hosts: %v", hosts)
	}
}

func TestEvaluateListsAllMatches(t *testing.T) {
	s := Snapshot{Version: 1, Rules: []Rule{
		{ID: "a", Effect: Allow, Host: "api.example.com", Methods: []string{"POST"}, Path: "/v1/*", SecretRef: "p1"},
		{ID: "b", Effect: Allow, Host: "api.example.com", Methods: []string{"POST"}, Path: "/v1/messages", SecretRef: "p2"},
	}}
	d := s.Evaluate(Request{Host: "api.example.com", Method: "POST", Path: "/v1/messages"})
	if !d.Allowed || d.RuleID != "a" || len(d.Matches) != 2 || d.Matches[1].SecretRef != "p2" {
		t.Fatalf("%+v", d)
	}
}

func TestValidPath(t *testing.T) {
	for value, want := range map[string]bool{"/": true, "/mcp": true, "/mcp/": true, "/v1/responses": true,
		"/mcp//": false, "//": false, "/a/../b": false, "/a/./b": false, "/a%2f": false, "relative": false, "": false} {
		if got := ValidPath(value); got != want {
			t.Errorf("%q: got %v want %v", value, got, want)
		}
	}
}

// Segment wildcards express git smart HTTP on GitHub: any owner and repository, one fixed tail.
func TestSegmentWildcards(t *testing.T) {
	snapshot := Snapshot{Version: 1, Rules: []Rule{
		{ID: "refs", Effect: Allow, Host: "github.com", Methods: []string{"GET"}, Path: "/*/*/info/refs", Anonymous: true},
		{ID: "fetch", Effect: Allow, Host: "github.com", Methods: []string{"POST"}, Path: "/*/*/git-upload-pack"},
		{ID: "archive", Effect: Allow, Host: "github.com", Methods: []string{"GET"}, Path: "/*/*/archive/*"},
		{ID: "push", Effect: Deny, Host: "github.com", Path: "/*/*/git-receive-pack"},
	}}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path string
		allowed      bool
		rule         string
	}{
		{"GET", "/octo/demo.git/info/refs", true, "refs"},
		{"GET", "/octo/demo/info/refs", true, "refs"},
		{"POST", "/octo/demo.git/git-upload-pack", true, "fetch"},
		{"GET", "/octo/demo/archive/refs/heads/main.zip", true, "archive"},
		{"GET", "/octo/demo/archive", true, "archive"},
		{"POST", "/octo/demo.git/git-receive-pack", false, "push"},
		{"GET", "/octo/info/refs", false, ""},
		{"GET", "/a/b/c/info/refs", false, ""},
		{"GET", "/octo/demo/info/refs/x", false, ""},
		{"GET", "/octo/demo.git/HEAD", false, ""},
		{"POST", "/octo/demo.git/info/refs", false, ""},
	} {
		d := snapshot.Evaluate(Request{Host: "github.com", Method: c.method, Path: c.path})
		if d.Allowed != c.allowed || d.RuleID != c.rule {
			t.Errorf("%s %s: %+v", c.method, c.path, d)
		}
	}
	if d := snapshot.Evaluate(Request{Host: "github.com", Method: "GET", Path: "/octo/demo/info/refs"}); !d.Anonymous {
		t.Fatal("the anonymous flag of the matching rule must reach the gatekeeper")
	}
}
