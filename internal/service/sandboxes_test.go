package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaoxier/AIofCage/internal/sandbox"
)

func TestPrepareProject(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "CageWorkspace")
	os.MkdirAll(filepath.Join(workspace, "existing"), 0o755)

	if got, err := prepareProject(ProjectUse, filepath.Join(workspace, "existing"), "x", nil); err != nil || got != filepath.Join(workspace, "existing") {
		t.Fatalf("use: %q %v", got, err)
	}
	outside := filepath.Join(home, "code", "web app")
	os.MkdirAll(filepath.Join(outside, "src"), 0o755)
	os.WriteFile(filepath.Join(outside, "src", "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(home, "secret.txt"), []byte("host only"), 0o600)
	os.Symlink(filepath.Join(home, "secret.txt"), filepath.Join(outside, "link-to-secret"))
	if _, err := prepareProject(ProjectUse, outside, "x", nil); err == nil || !strings.Contains(err.Error(), "复制到工作区") {
		t.Fatalf("outside folder must be rejected for use: %v", err)
	}
	copied, err := prepareProject(ProjectCopy, outside, "x", nil)
	if err != nil || copied != filepath.Join(workspace, "web-app") {
		t.Fatalf("copy: %q %v", copied, err)
	}
	if data, err := os.ReadFile(filepath.Join(copied, "src", "main.go")); err != nil || string(data) != "package main" {
		t.Fatalf("copied file: %q %v", data, err)
	}
	// Symlinks are copied as links, never followed: the secret's content is not in the workspace.
	if info, err := os.Lstat(filepath.Join(copied, "link-to-secret")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink not preserved as link: %v", err)
	}
	if again, _ := prepareProject(ProjectCopy, outside, "x", nil); again != filepath.Join(workspace, "web-app-2") {
		t.Fatalf("second copy should not overwrite: %q", again)
	}
	for _, refused := range []string{home, "/", workspace} {
		if _, err := prepareProject(ProjectCopy, refused, "x", nil); err == nil {
			t.Errorf("copying %s must be refused", refused)
		}
	}
	created, err := prepareProject(ProjectNew, "", "我的 网站.v2", nil)
	if err != nil || created != filepath.Join(workspace, "我的-网站-v2") {
		t.Fatalf("new: %q %v", created, err)
	}
	if info, err := os.Stat(created); err != nil || !info.IsDir() {
		t.Fatal("new folder not created")
	}
	if slug("../..") != "project" || slug("a/b") != "ab" {
		t.Fatalf("slug: %q %q", slug("../.."), slug("a/b"))
	}
}

// The project of a new sandbox may not overlap the project of an existing one. An existing folder
// is refused before anything is created; a new or copied folder skips names an old sandbox still
// claims although its folder is gone.
func TestPrepareProjectKeepsProjectsApart(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "CageWorkspace")
	for _, dir := range []string{"proj/sub", "proj-2"} {
		os.MkdirAll(filepath.Join(workspace, dir), 0o755)
	}
	existing := []sandbox.Sandbox{
		{ID: "sbx-outer", Name: "外层", ProjectPath: filepath.Join(workspace, "proj")},
		{ID: "sbx-gone", Name: "旧沙箱", ProjectPath: filepath.Join(workspace, "demo")}, // its folder was removed
		{ID: "sbx-nested", Name: "旧嵌套", ProjectPath: filepath.Join(workspace, "web-app", "src")},
	}
	for path, want := range map[string]string{
		filepath.Join(workspace, "proj", "sub"):       "这个文件夹在沙箱「外层」的项目里面，请换一个与它并列的文件夹",
		filepath.Join(workspace, "proj", "sub") + "/": "在沙箱「外层」的项目里面",
		filepath.Join(workspace, "proj"):              "这个文件夹已是沙箱「外层」的项目",
	} {
		if _, err := prepareProject(ProjectUse, path, "x", existing); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("use %s: %v, want %q", path, err, want)
		}
	}
	if got, err := prepareProject(ProjectUse, filepath.Join(workspace, "proj-2"), "x", existing); err != nil || got != filepath.Join(workspace, "proj-2") {
		t.Fatalf("a sibling sharing the prefix: %q %v", got, err)
	}
	if got, err := prepareProject(ProjectNew, "", "Demo", existing); err != nil || got != filepath.Join(workspace, "Demo-2") {
		t.Fatalf("new folder named like the removed project: %q %v", got, err)
	}
	outside := filepath.Join(home, "code", "web app")
	os.MkdirAll(outside, 0o755)
	if got, err := prepareProject(ProjectCopy, outside, "x", existing); err != nil || got != filepath.Join(workspace, "web-app-2") {
		t.Fatalf("copy named like the folder of a nested project: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "demo")); err == nil {
		t.Fatal("the removed project's folder was created again")
	}
}

func TestSandboxActionsNeedRuntime(t *testing.T) {
	app, _, _ := newApp(t)
	if _, err := app.CreateSandbox(CreateSandboxRequest{Name: "x"}); err == nil || !strings.Contains(err.Error(), "运行环境不可用") {
		t.Fatalf("create without runtime: %v", err)
	}
	if err := app.StartSandbox("x"); err == nil {
		t.Fatal("start without runtime")
	}
}

func TestTerminalCommandQuoting(t *testing.T) {
	args := []string{"/usr/local/bin/docker", "--context", "colima-aiofcage", "exec", "--workdir", "/Users/u/CageWorkspace/it's here", "x", "codex"}
	command := systemTerminalScript(`a'b"c\d`, args)
	// The shell must see exactly the original arguments.
	out, err := exec.Command("/bin/sh", "-c", "set -- "+strings.SplitN(command, "; clear; ", 2)[1]+`; printf '%s\n' "$@"`).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"); strings.Join(got, "|") != strings.Join(args, "|") {
		t.Fatalf("shell saw %q", got)
	}
	if !strings.Contains(command, `'AIofCage · ab"cd'`) {
		t.Fatalf("title not sanitized: %s", command)
	}
	if _, err := exec.LookPath("osascript"); err != nil {
		t.Skip("osascript not available")
	}
	// AppleScript must reproduce the exact command string (checked without opening Terminal).
	echoed, err := exec.Command("osascript", "-e", "return "+appleScriptString(command)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSuffix(string(echoed), "\n") != command {
		t.Fatalf("AppleScript changed the command:\n%s\n%s", echoed, command)
	}
}

func TestMCPServiceLifecycle(t *testing.T) {
	app, secrets, st := newApp(t)
	view, err := app.SaveMCPService(SaveMCPServiceRequest{Name: "GitHub", URL: "https://api.githubcopilot.com/mcp/", Preset: "github", Token: "ghtoken-123"})
	if err != nil || view.ID != "github" || !view.KeySaved || string(secrets["aiofcage-mcp-github"]) != "ghtoken-123" {
		t.Fatalf("save: %+v %v", view, err)
	}
	second, err := app.SaveMCPService(SaveMCPServiceRequest{Name: "Another GitHub", URL: "https://api.githubcopilot.com/mcp/", Preset: "github", Token: "synthetic-token-2"})
	if err != nil || second.ID == "github" || !strings.HasPrefix(second.ID, "mcp-") {
		t.Fatalf("second id: %+v %v", second, err)
	}
	if _, err := app.SaveMCPService(SaveMCPServiceRequest{Name: "x", URL: "https://example.com/mcp"}); err == nil {
		t.Fatal("new service without token accepted")
	}
	st.SaveSandbox(sandbox.Sandbox{ID: "sbx-1", Name: "demo", MCP: []string{"github"}})
	if err := app.DeleteMCPService("github"); err == nil || !strings.Contains(err.Error(), "demo") {
		t.Fatalf("in-use service deleted: %v", err)
	}
	st.DeleteSandbox("sbx-1")
	if err := app.DeleteMCPService("github"); err != nil {
		t.Fatal(err)
	}
	if _, ok := secrets["aiofcage-mcp-github"]; ok {
		t.Fatal("token not removed")
	}
	items, _ := app.ListMCPServices()
	for _, item := range items {
		if strings.Contains(string(mustJSON(t, item)), "synthetic-token-2") {
			t.Fatal("view leaked the token")
		}
	}
}
