package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaoxier/AIofCage/internal/sandbox"
)

// Every way of creating a sandbox (the desktop app, cagectl, demo cases, the local AI control)
// goes through Create, which refuses a project that overlaps another sandbox's project.
func TestCreateKeepsProjectsApart(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "CageWorkspace")
	for _, dir := range []string{"proj/sub", "proj-2", "team/api"} {
		if err := os.MkdirAll(filepath.Join(workspace, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m, st, _ := newRemoteManager(t, nil)
	create := func(name, project string) error {
		_, err := m.Create(context.Background(), CreateRequest{Name: name, ProjectPath: project})
		return err
	}
	if err := create("外层", filepath.Join(workspace, "proj")); err != nil {
		t.Fatal(err)
	}
	if err := create("深层", filepath.Join(workspace, "team", "api")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ project, want string }{
		{filepath.Join(workspace, "proj", "sub"), "这个文件夹在沙箱「外层」的项目里面，请换一个与它并列的文件夹"},
		{filepath.Join(workspace, "proj", "sub") + "/", "在沙箱「外层」的项目里面"},
		{filepath.Join(workspace, "proj") + "/", "已是沙箱「外层」的项目"},
		{filepath.Join(workspace, "team"), "里面有沙箱「深层」的项目"},
	} {
		if err := create("新的", c.project); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.project, err, c.want)
		}
	}
	if err := create("并列", filepath.Join(workspace, "proj-2")+"/"); err != nil {
		t.Fatalf("a sibling sharing the prefix is refused: %v", err)
	}
	items, _ := st.Sandboxes()
	if len(items) != 3 || items[2].ProjectPath != filepath.Join(workspace, "proj-2") {
		t.Fatalf("saved sandboxes: %+v", items)
	}
}

// Sandboxes created before the rule may overlap; neither of the two starts, while a sandbox apart
// from them gets past the check (to the fake VM, which fails).
func TestStartRefusesOverlappingProjects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, st, _ := newRemoteManager(t, nil)
	legacy := []sandbox.Sandbox{
		{ID: "sbx-outer001", Name: "外层", ProjectPath: "/Users/u/CageWorkspace/proj"},
		{ID: "sbx-inner001", Name: "内层", ProjectPath: "/Users/u/CageWorkspace/proj/sub"},
		{ID: "sbx-apart001", Name: "并列", ProjectPath: "/Users/u/CageWorkspace/proj-2"},
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(st.Dir(), "sandboxes.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := script(t, "exit 1\n")
	m.env = &Environment{colima: runner{path: fake}, limactl: fake, docker: runner{path: fake}, store: st}
	var steps []string
	m.Progress = func(id string, update Update) {
		if update.Step != "" {
			steps = append(steps, id+" "+update.Step)
		}
	}
	for id, other := range map[string]string{"sbx-inner001": "「外层」", "sbx-outer001": "「内层」"} {
		err := m.Start(context.Background(), id)
		if err == nil || !strings.HasPrefix(err.Error(), "不能启动：项目文件夹与沙箱"+other+"的项目重叠") || !strings.Contains(err.Error(), "项目文件夹会保留") {
			t.Errorf("%s: %v", id, err)
		}
	}
	if len(steps) != 0 {
		t.Fatalf("a refused start began: %v", steps)
	}
	if err := m.Start(context.Background(), "sbx-apart001"); err == nil || strings.Contains(err.Error(), "不能启动") {
		t.Fatalf("a sandbox apart from the others: %v", err)
	}
	if len(steps) == 0 || steps[0] != "sbx-apart001 检查专用虚拟机" {
		t.Fatalf("the sandbox apart did not get past the check: %v", steps)
	}
}
