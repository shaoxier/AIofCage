package service

import (
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/sandbox"
)

// projectFixture is a project the agent filled with tricks: links out of the project, a FIFO, a
// script disguised as a picture, hidden files and a huge folder.
func projectFixture(t *testing.T) (*App, string) {
	t.Helper()
	app, _, st := newApp(t)
	dir := filepath.Join(testWorkspace(t), "proj")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "id_rsa")
	write := func(name, content string) {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(outside, []byte("synthetic-private-key"), 0o600)
	write("README.md", "# 项目\n<script>alert(1)</script>\n")
	write("src/main.go", "package main\n")
	write(".env", "TOKEN=cage-placeholder\n")
	write("logo.svg", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	write("pic.png", string(pngHeader(64, 64)))
	write("data.bin", "ab\x00cd")
	write("big.txt", strings.Repeat(strings.Repeat("汉", 99)+"\n", projectPreviewLimit/298+10)) // the cut falls inside a character
	for i := 0; i <= projectListLimit; i++ {
		write(fmt.Sprintf("node_modules/p%04d", i), "")
	}
	os.Symlink(outside, filepath.Join(dir, "key-link"))
	os.Symlink("src", filepath.Join(dir, "src-link"))
	os.Symlink("../../..", filepath.Join(dir, "src", "up"))
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	write("dir\\name/back\\slash.txt", "ok")
	write("min.js", strings.Repeat("x", projectLineLimit*3)+"\n短行\n")
	if err := st.SaveSandbox(sandbox.Sandbox{ID: "sbx-files001", Name: "files", ProjectPath: dir}); err != nil {
		t.Fatal(err)
	}
	return app, outside
}

// testWorkspace points the home folder at a temporary one and returns its ~/CageWorkspace, with
// links resolved like a saved project path.
func testWorkspace(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "CageWorkspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestProjectListingShowsTheProjectOnly(t *testing.T) {
	app, _ := projectFixture(t)
	listing, err := app.ListProjectEntries("sbx-files001", "", false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	kinds := map[string]string{}
	for _, entry := range listing.Entries {
		names = append(names, entry.Name)
		kinds[entry.Name] = entry.Kind
	}
	if strings.Join(names, ",") != "dir\\name,node_modules,src,big.txt,data.bin,key-link,logo.svg,min.js,pic.png,pipe,README.md,src-link" {
		t.Fatalf("folders first, then by name, hidden files left out: %v", names)
	}
	if listing.Hidden != 1 || kinds["key-link"] != "link" || kinds["src-link"] != "link" || kinds["pipe"] != "other" {
		t.Fatalf("hidden %d, kinds %v", listing.Hidden, kinds)
	}
	if shown, _ := app.ListProjectEntries("sbx-files001", "", true); len(shown.Entries) != len(listing.Entries)+1 {
		t.Fatal("hidden files are listed on request")
	}
	modules, err := app.ListProjectEntries("sbx-files001", "node_modules", false)
	if err != nil || len(modules.Entries) != projectListLimit || !modules.More || modules.Entries[0].Path != "node_modules/p0000" {
		t.Fatalf("a huge folder is cut at the limit: %d %v %v", len(modules.Entries), modules.More, err)
	}
	for _, dir := range []string{"..", "/etc", "src/../..", "src/up", "src-link", "key-link", `src\..\..`, "src/main.go"} {
		if _, err := app.ListProjectEntries("sbx-files001", dir, false); err == nil {
			t.Errorf("%q must not be listed", dir)
		}
	}
}

func TestProjectPreviewNeverLeavesTheProject(t *testing.T) {
	app, outside := projectFixture(t)
	read := func(name string) ProjectFile {
		t.Helper()
		done := make(chan ProjectFile, 1)
		go func() {
			file, err := app.ReadProjectFile("sbx-files001", name)
			if err != nil {
				file.Kind = "error: " + err.Error()
			}
			done <- file
		}()
		select {
		case file := <-done:
			return file
		case <-time.After(5 * time.Second):
			t.Fatalf("reading %s hangs", name)
			return ProjectFile{}
		}
	}
	if file := read("README.md"); file.Kind != "text" || !strings.Contains(file.Text, "<script>") {
		t.Fatalf("text comes back as it is, for the page to show as plain text: %+v", file)
	}
	if file := read("logo.svg"); file.Kind != "text" || file.Image != "" {
		t.Fatalf("SVG is never a picture: %+v", file)
	}
	if file := read("pic.png"); file.Kind != "image" || !strings.HasPrefix(file.Image, "data:image/png;base64,") {
		t.Fatalf("%+v", file)
	}
	if file := read("data.bin"); file.Kind != "binary" || file.Text != "" {
		t.Fatalf("%+v", file)
	}
	big := read("big.txt")
	if big.Kind != "text" || !big.Truncated || len(big.Text) > projectPreviewLimit || !strings.HasSuffix(big.Text, "汉") {
		t.Fatalf("a long file is cut at a character boundary: %s %v %d", big.Kind, big.Truncated, len(big.Text))
	}
	for _, name := range []string{"key-link", "src-link"} {
		if file := read(name); file.Kind != "link" || file.Text != "" {
			t.Fatalf("links are never followed: %s %+v", name, file)
		}
	}
	if file := read("pipe"); file.Kind != "other" {
		t.Fatalf("a FIFO is not read: %+v", file)
	}
	if file := read(`dir\name/back\slash.txt`); file.Kind != "text" || file.Text != "ok" {
		t.Fatalf("a backslash is an ordinary character in a file name: %+v", file)
	}
	if file := read("min.js"); !file.Clipped || file.Truncated || !strings.HasSuffix(file.Text, " …\n短行\n") ||
		len(strings.SplitN(file.Text, "\n", 2)[0]) != projectLineLimit+len(" …") {
		t.Fatalf("an endless line is cut, the rest kept: %v %v %q", file.Clipped, file.Truncated, file.Text[len(file.Text)-20:])
	}
	for _, name := range []string{"../" + filepath.Base(outside), outside, "src/up/" + filepath.Base(filepath.Dir(outside)), "", ".", "src"} {
		if file := read(name); !strings.HasPrefix(file.Kind, "error") && file.Kind != "other" {
			t.Fatalf("%q must not be read: %+v", name, file)
		}
		if file := read(name); strings.Contains(file.Text, "synthetic-private-key") {
			t.Fatalf("%q leaked a file outside the project", name)
		}
	}
}

// A project inside another sandbox's project can be replaced by that sandbox's agent: with a link
// out of the workspace, a FIFO, or a link one folder further up. The panel must neither follow
// nor block; and a folder opened before the swap stays the folder that is read.
func TestNestedProjectCannotBeSwapped(t *testing.T) {
	app, _, st := newApp(t)
	workspace := testWorkspace(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("synthetic-secret"), 0o600)
	inner := filepath.Join(workspace, "outer", "sub", "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(inner, "a.txt"), []byte("inside"), 0o644)
	if err := st.SaveSandbox(sandbox.Sandbox{ID: "sbx-inner001", Name: "inner", ProjectPath: inner}); err != nil {
		t.Fatal(err)
	}
	list := func() error {
		done := make(chan error, 1)
		go func() { _, err := app.ListProjectEntries("sbx-inner001", "", true); done <- err }()
		select {
		case err := <-done:
			return err
		case <-time.After(3 * time.Second):
			t.Fatal("listing hangs")
			return nil
		}
	}
	if err := list(); err != nil {
		t.Fatal(err)
	}
	moved := inner + ".moved"
	os.Rename(inner, moved)
	os.Symlink(outside, inner)
	if err := list(); err == nil {
		t.Fatal("a project replaced with a link must not be read")
	}
	if file, err := app.ReadProjectFile("sbx-inner001", "secret.txt"); err == nil || strings.Contains(file.Text, "synthetic") {
		t.Fatalf("read through the link: %+v %v", file, err)
	}
	os.Remove(inner)
	if err := syscall.Mkfifo(inner, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := list(); err == nil {
		t.Fatal("a project replaced with a FIFO must fail, not block")
	}
	os.Remove(inner)
	os.Rename(moved, inner)
	// A link one level up: outer/sub → outside, with an "inner" folder there.
	os.MkdirAll(filepath.Join(outside, "inner"), 0o755)
	os.WriteFile(filepath.Join(outside, "inner", "secret.txt"), []byte("synthetic-secret"), 0o600)
	sub := filepath.Join(workspace, "outer", "sub")
	os.Rename(sub, sub+".moved")
	os.Symlink(outside, sub)
	if file, err := app.ReadProjectFile("sbx-inner001", "secret.txt"); err == nil || strings.Contains(file.Text, "synthetic") {
		t.Fatalf("read through a link further up: %+v %v", file, err)
	}
	os.Remove(sub)
	os.Rename(sub+".moved", sub)
	// Once opened, the root stays on the folder it opened.
	root, err := openProjectRoot(workspace, inner)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	os.Rename(inner, moved)
	os.Symlink(filepath.Join(outside, "inner"), inner)
	if data, err := root.ReadFile("a.txt"); err != nil || string(data) != "inside" {
		t.Fatalf("the open root moved with the path: %q %v", data, err)
	}
	if _, err := root.ReadFile("secret.txt"); err == nil {
		t.Fatal("the open root reads the swapped-in folder")
	}
}

func TestHiddenEntriesDoNotCrowdOutTheListing(t *testing.T) {
	app, _, st := newApp(t)
	dir := filepath.Join(testWorkspace(t), "dots")
	os.Mkdir(dir, 0o755)
	for i := 0; i < projectListLimit+500; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf(".cache%05d", i)), nil, 0o644)
	}
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		os.WriteFile(filepath.Join(dir, name), nil, 0o644)
	}
	st.SaveSandbox(sandbox.Sandbox{ID: "sbx-dots0001", Name: "dots", ProjectPath: dir})
	listing, err := app.ListProjectEntries("sbx-dots0001", "", false)
	if err != nil || len(listing.Entries) != 3 || listing.Hidden != projectListLimit+500 || listing.More {
		t.Fatalf("visible files are listed however many hidden ones there are: %d %d %v %v", len(listing.Entries), listing.Hidden, listing.More, err)
	}
}

// A picture that is small on disk can declare dimensions the page would choke on decoding.
func TestHugePicturesAreNotShown(t *testing.T) {
	app, _, st := newApp(t)
	dir := filepath.Join(testWorkspace(t), "pics")
	os.Mkdir(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "huge.png"), pngHeader(20000, 20000), 0o644)
	os.WriteFile(filepath.Join(dir, "small.png"), pngHeader(64, 64), 0o644)
	st.SaveSandbox(sandbox.Sandbox{ID: "sbx-pics0001", Name: "pics", ProjectPath: dir})
	if file, err := app.ReadProjectFile("sbx-pics0001", "huge.png"); err != nil || file.Kind != "large" || file.Image != "" {
		t.Fatalf("%+v %v", file, err)
	}
	if file, err := app.ReadProjectFile("sbx-pics0001", "small.png"); err != nil || file.Kind != "image" {
		t.Fatalf("%+v %v", file, err)
	}
}

// pngHeader is the start of a PNG: enough to read its dimensions.
func pngHeader(width, height uint32) []byte {
	ihdr := []byte{0, 0, 0, 13, 'I', 'H', 'D', 'R', byte(width >> 24), byte(width >> 16), byte(width >> 8), byte(width),
		byte(height >> 24), byte(height >> 16), byte(height >> 8), byte(height), 8, 2, 0, 0, 0}
	crc := crc32.ChecksumIEEE(ihdr[4:])
	return append(append([]byte("\x89PNG\r\n\x1a\n"), ihdr...), byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}
