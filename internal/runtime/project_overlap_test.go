package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
