package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/sandbox"
)

func TestSaveSandboxKeepsProjectsApart(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSandbox(sandbox.Sandbox{ID: "sbx-a", Name: "a", ProjectPath: "/Users/u/CageWorkspace/proj"}); err != nil {
		t.Fatal(err)
	}
	for project, want := range map[string]string{
		"/Users/u/CageWorkspace/proj":       "已是沙箱「a」的项目",
		"/Users/u/CageWorkspace/proj/":      "已是沙箱「a」的项目",
		"/Users/u/CageWorkspace/proj/sub":   "在沙箱「a」的项目里面",
		"/Users/u/CageWorkspace/PROJ/sub/":  "在沙箱「a」的项目里面",
		"/Users/u/CageWorkspace":            "里面有沙箱「a」的项目",
		"/Users/u/CageWorkspace/proj/a/b/c": "在沙箱「a」的项目里面",
	} {
		if err := st.SaveSandbox(sandbox.Sandbox{ID: "sbx-b", Name: "b", ProjectPath: project}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", project, err, want)
		}
	}
	if err := st.SaveSandbox(sandbox.Sandbox{ID: "sbx-b", Name: "b", ProjectPath: "/Users/u/CageWorkspace/proj-2"}); err != nil {
		t.Fatalf("a sibling sharing the prefix is refused: %v", err)
	}
	if items, _ := st.Sandboxes(); len(items) != 2 {
		t.Fatalf("refused sandboxes were saved: %+v", items)
	}

	// Sandboxes saved before the rule may overlap. They can still be saved while they keep their
	// project; moving one to another overlapping folder is refused.
	legacy := []sandbox.Sandbox{
		{ID: "sbx-outer", Name: "outer", ProjectPath: "/Users/u/CageWorkspace/old"},
		{ID: "sbx-inner", Name: "inner", ProjectPath: "/Users/u/CageWorkspace/old/inner"},
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(st.Dir(), "sandboxes.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	inner := legacy[1]
	inner.Limits.MaxRequests = 5
	if err := st.SaveSandbox(inner); err != nil {
		t.Fatalf("a legacy sandbox that keeps its project: %v", err)
	}
	inner.ProjectPath = "/Users/u/CageWorkspace/old/other"
	if err := st.SaveSandbox(inner); err == nil || !strings.Contains(err.Error(), "在沙箱「outer」的项目里面") {
		t.Fatalf("moving into another sandbox's project: %v", err)
	}
	if saved, _ := st.Sandbox("sbx-inner"); saved.ProjectPath != "/Users/u/CageWorkspace/old/inner" || saved.Limits.MaxRequests != 5 {
		t.Fatalf("legacy sandbox after the saves: %+v", saved)
	}
}

func TestRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if items, err := st.Providers(); err != nil || len(items) != 0 {
		t.Fatalf("empty store: %v %v", items, err)
	}
	p := provider.Provider{ID: "a", Name: "A", CreatedAt: time.Now()}
	if err := st.SaveProvider(p); err != nil {
		t.Fatal(err)
	}
	p.Name = "A2"
	if err := st.SaveProvider(p); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Provider("a"); err != nil || got.Name != "A2" {
		t.Fatalf("replace: %+v %v", got, err)
	}
	s1 := sandbox.Sandbox{ID: "sbx-1", Name: "one"}
	if err := st.SaveSandbox(s1); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSandbox(sandbox.Sandbox{ID: "sbx-2", Name: "one"}); err == nil {
		t.Fatal("duplicate sandbox name accepted")
	}
	if got, err := st.Sandbox("one"); err != nil || got.ID != "sbx-1" {
		t.Fatalf("lookup by name: %+v %v", got, err)
	}
	if err := st.DeleteSandbox("sbx-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Sandbox("sbx-1"); err == nil {
		t.Fatal("deleted sandbox still present")
	}
	for _, name := range []string{"providers.json", "sandboxes.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions: %v %v", name, info.Mode(), err)
		}
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}
