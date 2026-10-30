package mcp

import (
	"strings"
	"testing"
)

func TestRemoteServiceGoesThroughTheGateway(t *testing.T) {
	s := Service{ID: "github", Name: "GitHub", URL: "https://API.githubcopilot.com/mcp/", KeyRef: KeyRefFor("github")}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if upstream, err := s.Upstream(); err != nil || upstream != "https://api.githubcopilot.com/mcp/" {
		t.Fatalf("upstream %q %v", upstream, err)
	}
	if s.AgentURL() != "http://gatekeeper:8890/mcp/github" {
		t.Fatalf("agents must reach remote services through the gateway, got %s", s.AgentURL())
	}
	if EnvVar("my-svc") != "AIOFCAGE_MCP_MY_SVC_TOKEN" {
		t.Fatal(EnvVar("my-svc"))
	}
	for _, bad := range []Service{{ID: "Bad", Name: "x", URL: s.URL, KeyRef: "k"}, {ID: "x", Name: "x", URL: "http://a.example/mcp", KeyRef: "k"},
		{ID: "x", Name: "x", URL: "https://1.2.3.4/mcp", KeyRef: "k"}, {ID: "x", Name: "", URL: s.URL, KeyRef: "k"},
		{ID: "x", Name: "x", URL: s.URL, KeyRef: "k", Auth: "basic"}, {ID: "x", Name: "x", Kind: KindOAuth, URL: s.URL, KeyRef: "k", Auth: AuthNone},
		{ID: "x", Name: "x", URL: s.URL, KeyRef: "k", Catalog: []Tool{{Name: "a b"}}},
		{ID: "x", Name: "x", URL: s.URL, KeyRef: "k", Catalog: []Tool{{Name: "a"}, {Name: "a"}}}} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	public := Service{ID: "docs", Name: "Docs", URL: "https://docs.example.com/mcp", KeyRef: KeyRefFor("docs"), Auth: AuthNone}
	if err := public.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(Presets()) == 0 {
		t.Fatal("no presets")
	}
}

func TestRemoteToolGrantsDefaultToReadOnly(t *testing.T) {
	s := Service{ID: "github", Name: "GitHub", URL: "https://api.githubcopilot.com/mcp/", KeyRef: KeyRefFor("github"),
		Catalog: []Tool{{Name: "get_issue", ReadOnly: true}, {Name: "create_issue"}, {Name: "search_code", ReadOnly: true}}}
	if got := strings.Join(s.Tools(), ","); got != "get_issue,create_issue,search_code" {
		t.Fatalf("tools %s", got)
	}
	if got := strings.Join(s.DefaultTools(), ","); got != "get_issue,search_code" {
		t.Fatalf("defaults must be the tools the service marks read-only: %s", got)
	}
	if err := ValidateTools(s, []string{"create_issue"}); err != nil {
		t.Fatalf("a listed tool may be granted explicitly: %v", err)
	}
	if err := ValidateTools(s, []string{"delete_repo"}); err == nil {
		t.Fatal("a tool the service did not list cannot be granted")
	}
	empty := Service{ID: "new", Name: "New", URL: "https://mcp.example.com/mcp", KeyRef: "k"}
	if len(empty.Tools()) != 0 || len(empty.DefaultTools()) != 0 {
		t.Fatal("a service whose tools were never listed grants nothing")
	}
}

func TestDatabaseURL(t *testing.T) {
	clean, password, err := SplitDatabaseURL(KindPostgres, " postgresql://app:p%40ss@DB.Example.com:6543/shop?sslmode=require ")
	if err != nil || clean != "postgresql://app@db.example.com:6543/shop?sslmode=require" || password != "p@ss" {
		t.Fatalf("%q %q %v", clean, password, err)
	}
	if clean, password, err := SplitDatabaseURL(KindPostgres, "postgres://app@localhost/shop"); err != nil || clean != "postgres://app@localhost/shop" || password != "" {
		t.Fatalf("%q %q %v", clean, password, err)
	}
	for _, bad := range []string{"mysql://a@h/db", "postgresql://h/db", "postgresql://a@h/", "postgresql://a@h:99999/db",
		"postgresql://a@h/db?host=/tmp", "postgresql://a@h/db?sslrootcert=/etc/x", "postgresql://a@h/db?sslmode=maybe",
		"postgresql://a@bad_host/db", "postgresql://a@h/db/x", "postgresql://a@h/db#x"} {
		if _, _, err := SplitDatabaseURL(KindPostgres, bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestConnectorService(t *testing.T) {
	s := Service{ID: "shop-db", Name: "商城数据库", Kind: KindPostgres, URL: "postgresql://app@localhost/shop?sslmode=disable", KeyRef: KeyRefFor("shop-db")}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.AgentURL() != "http://gatekeeper:8890/mcp/shop-db" || len(s.Tools()) != 1 || s.Tools()[0] != "query" ||
		len(s.DefaultTools()) != 1 {
		t.Fatalf("url %s tools %v", s.AgentURL(), s.Tools())
	}
	command, redact, err := s.Launch("p@ss word")
	if err != nil || len(command) != 2 || command[0] != "/opt/aiofcage-postgres/server.mjs" ||
		command[1] != "postgresql://app:p%40ss%20word@host.lima.internal:5432/shop?sslmode=disable" {
		t.Fatalf("%v %v", command, err)
	}
	if len(redact) != 2 || redact[0] != "p@ss word" || redact[1] != "p%40ss%20word" {
		t.Fatalf("redact %v", redact)
	}
	withPassword := s
	withPassword.URL = "postgresql://app:secret@localhost/shop"
	if withPassword.Validate() == nil {
		t.Fatal("a stored URL must never contain the password")
	}
	unknown := s
	unknown.Kind = "redis"
	if unknown.Validate() == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestOAuthService(t *testing.T) {
	s := Service{ID: "notion", Name: "Notion", Kind: KindOAuth, URL: "https://mcp.notion.com/mcp", KeyRef: KeyRefFor("notion")}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.IsConnector() || !s.IsOAuth() || s.AgentURL() != GatewayURL("notion") {
		t.Fatal("oauth services are remote HTTP services reached through the gateway")
	}
	s.URL = "http://mcp.notion.com/mcp"
	if s.Validate() == nil {
		t.Fatal("OAuth services must use https")
	}
}

func TestValidateTools(t *testing.T) {
	postgres := Service{ID: "db", Name: "数据库", Kind: KindPostgres, URL: "postgresql://app@localhost/shop", KeyRef: KeyRefFor("db")}
	for _, tc := range []struct {
		name  string
		tools []string
		want  bool
	}{
		{name: "catalog tool", tools: []string{"query"}, want: true},
		{name: "explicit empty grant", tools: []string{}, want: true},
		{name: "unknown tool", tools: []string{"drop_table"}},
		{name: "duplicate tool", tools: []string{"query", "query"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateTools(postgres, tc.tools); (err == nil) != tc.want {
				t.Fatalf("ValidateTools(%v) error = %v, want valid = %v", tc.tools, err, tc.want)
			}
		})
	}
	httpService := Service{ID: "github", Name: "GitHub", Kind: KindHTTP, URL: "https://api.githubcopilot.com/mcp/", KeyRef: KeyRefFor("github")}
	if err := ValidateTools(httpService, []string{"query"}); err == nil {
		t.Fatal("a remote service without a tool list cannot grant tools")
	}
	if err := ValidateTools(httpService, nil); err != nil {
		t.Fatalf("an empty remote grant is valid: %v", err)
	}
}
