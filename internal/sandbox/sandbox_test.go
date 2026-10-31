package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaoxier/AIofCage/internal/session"
)

func TestValidateProject(t *testing.T) {
	home := t.TempDir()
	home, _ = filepath.EvalSymlinks(home)
	t.Setenv("HOME", home)
	project := filepath.Join(home, "CageWorkspace", "demo")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(home, "elsewhere")
	os.MkdirAll(outside, 0o755)
	link := filepath.Join(home, "CageWorkspace", "link")
	os.Symlink(outside, link)
	if err := (Sandbox{ID: NewID(), Name: "空白", ProjectPath: project}).Validate(); err != nil {
		t.Fatalf("空白沙箱被拒绝: %v", err)
	}
	if err := ValidateProject(project); err != nil {
		t.Fatalf("valid project rejected: %v", err)
	}
	for name, path := range map[string]string{
		"outside":   outside,
		"workspace": filepath.Join(home, "CageWorkspace"),
		"symlink":   link,
		"relative":  "CageWorkspace/demo",
		"missing":   filepath.Join(home, "CageWorkspace", "nope"),
		"dotdot":    filepath.Join(home, "CageWorkspace", "demo", "..", "..", "elsewhere"),
	} {
		if ValidateProject(path) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestCheckProjectOverlap(t *testing.T) {
	existing := []Sandbox{
		{ID: "sbx-outer", Name: "外层", ProjectPath: "/Users/u/CageWorkspace/proj"},
		{ID: "sbx-deep", Name: "深层", ProjectPath: "/Users/u/CageWorkspace/team/api/"},
		{ID: "sbx-blank", Name: "无项目"},
	}
	for _, c := range []struct{ project, want string }{
		{"/Users/u/CageWorkspace/proj/sub", "这个文件夹在沙箱「外层」的项目里面，请换一个与它并列的文件夹"},
		{"/Users/u/CageWorkspace/proj/sub/deeper/", "在沙箱「外层」的项目里面"},
		{"/Users/u/CageWorkspace/Proj/SUB", "在沙箱「外层」的项目里面"}, // the Mac ignores case by default
		{"/Users/u/CageWorkspace/proj", "这个文件夹已是沙箱「外层」的项目，一个文件夹只能给一个沙箱用"},
		{"/Users/u/CageWorkspace/proj/", "已是沙箱「外层」的项目"},
		{"/Users/u/CageWorkspace/proj/.", "已是沙箱「外层」的项目"},
		{"/Users/u/CageWorkspace/team", "这个文件夹里面有沙箱「深层」的项目，请换一个不包含它的文件夹"},
		{"/Users/u/CageWorkspace/team/", "里面有沙箱「深层」的项目"},
		{"/Users/u/CageWorkspace/team/api", "已是沙箱「深层」的项目"},
		// Apart: siblings, names sharing a prefix, and folders beside a nested project.
		{"/Users/u/CageWorkspace/proj-2", ""},
		{"/Users/u/CageWorkspace/proj2", ""},
		{"/Users/u/CageWorkspace/pro", ""},
		{"/Users/u/CageWorkspace/proj-2/sub/", ""},
		{"/Users/u/CageWorkspace/team/web", ""},
		{"/Users/u/CageWorkspace/team/api-v2", ""},
		{"/Users/u/CageWorkspace/team-api", ""},
	} {
		err := CheckProjectOverlap("sbx-new", c.project, existing)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", c.project, err, c.want)
		}
	}
	// A sandbox is never compared with itself, so saving it again keeps its project.
	if err := CheckProjectOverlap("sbx-outer", "/Users/u/CageWorkspace/proj", existing); err != nil {
		t.Fatalf("own project refused: %v", err)
	}
	if err := CheckProjectOverlap("sbx-new", "", existing); err != nil {
		t.Fatalf("no project: %v", err)
	}
	if other, found := OverlappingSandbox("sbx-outer", "/Users/u/CageWorkspace/proj", existing); found {
		t.Fatalf("overlaps itself: %+v", other)
	}
	if other, found := OverlappingSandbox("", "/Users/u/CageWorkspace/team/api/src", existing); !found || other.ID != "sbx-deep" {
		t.Fatalf("nested project not found: %+v %v", other, found)
	}
}

// Folders that exist are also compared by identity: another path to the same folder overlaps too.
func TestProjectOverlapComparesFolders(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outer := filepath.Join(workspace, "proj")
	if err := os.MkdirAll(filepath.Join(outer, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outer, filepath.Join(workspace, "alias")); err != nil {
		t.Fatal(err)
	}
	existing := []Sandbox{{ID: "sbx-outer", Name: "外层", ProjectPath: outer}}
	if err := CheckProjectOverlap("", filepath.Join(workspace, "alias", "sub"), existing); err == nil || !strings.Contains(err.Error(), "在沙箱「外层」的项目里面") {
		t.Fatalf("a path through another name of the project: %v", err)
	}
	inner := []Sandbox{{ID: "sbx-inner", Name: "内层", ProjectPath: filepath.Join(workspace, "alias", "sub")}}
	if err := CheckProjectOverlap("", outer, inner); err == nil || !strings.Contains(err.Error(), "里面有沙箱「内层」的项目") {
		t.Fatalf("a project reached through another name: %v", err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "beside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckProjectOverlap("", filepath.Join(workspace, "beside"), existing); err != nil {
		t.Fatalf("a folder beside the project: %v", err)
	}
	t.Run("unicode forms", func(t *testing.T) {
		composed := filepath.Join(workspace, "café")
		if err := os.MkdirAll(filepath.Join(composed, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		decomposed := filepath.Join(workspace, "café")
		if _, err := os.Stat(decomposed); err != nil {
			t.Skip("this file system tells the two Unicode forms of a name apart")
		}
		existing := []Sandbox{{ID: "sbx-cafe", Name: "咖啡", ProjectPath: composed}}
		if err := CheckProjectOverlap("", filepath.Join(decomposed, "sub"), existing); err == nil || !strings.Contains(err.Error(), "在沙箱「咖啡」的项目里面") {
			t.Fatalf("the other Unicode form of the project's name: %v", err)
		}
	})
}

func TestSandboxHelpers(t *testing.T) {
	s := Sandbox{ID: NewID(), Name: "demo", Bindings: []Binding{{Agent: session.DSH}, {Agent: session.Codex, ProviderID: "a"}}}
	if !strings.HasPrefix(s.ID, "sbx-") || len(s.ID) != 12 {
		t.Fatalf("id %q", s.ID)
	}
	if got := s.Agents(); len(got) != 2 || got[0] != session.Codex || got[1] != session.DSH {
		t.Fatalf("agents %v", got)
	}
	if s.ProviderFor(session.Codex) != "a" || s.ProviderFor(session.ClaudeCode) != "" {
		t.Fatal("provider lookup")
	}
	a, b := NewPlaceholder(), NewPlaceholder()
	if a == b || !strings.HasPrefix(a, "cage-") || len(a) < 30 {
		t.Fatalf("placeholders %q %q", a, b)
	}
	s.Bindings = append(s.Bindings, Binding{Agent: session.Codex})
	if s.Validate() == nil {
		t.Fatal("duplicate agent accepted")
	}
}

func TestAddLiveCountsDeltasPerRun(t *testing.T) {
	var s Sandbox
	s.AddLive("run-a", map[string]ProviderUsage{"p": {Requests: 2, InputTokens: 10, OutputTokens: 5}}, 2, 15)
	s.AddLive("run-a", map[string]ProviderUsage{"p": {Requests: 3, InputTokens: 30, OutputTokens: 6}}, 3, 36)
	s.AddLive("run-a", map[string]ProviderUsage{"p": {Requests: 3, InputTokens: 30, OutputTokens: 6}}, 3, 36) // repeated poll
	if got := s.ProviderUsage["p"]; got != (ProviderUsage{3, 30, 6}) {
		t.Fatalf("run-a total %+v", got)
	}
	s.AddLive("run-b", map[string]ProviderUsage{"p": {Requests: 1, InputTokens: 4, OutputTokens: 1}, "q": {Requests: 1}}, 4, 41)
	if got := s.ProviderUsage["p"]; got != (ProviderUsage{4, 34, 7}) {
		t.Fatalf("after restart %+v", got)
	}
	if s.ProviderUsage["q"].Requests != 1 || s.Usage.Requests != 4 || s.Usage.Tokens != 41 {
		t.Fatalf("totals %+v %+v", s.ProviderUsage, s.Usage)
	}
}
