package service

import (
	"fmt"
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
	dir := t.TempDir()
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
	write("pic.png", "\x89PNG\r\n\x1a\nrest")
	write("data.bin", "ab\x00cd")
	write("big.txt", strings.Repeat("汉", projectPreviewLimit/3+100))
	for i := 0; i <= projectListLimit; i++ {
		write(fmt.Sprintf("node_modules/p%04d", i), "")
	}
	os.Symlink(outside, filepath.Join(dir, "key-link"))
	os.Symlink("src", filepath.Join(dir, "src-link"))
	os.Symlink("../../..", filepath.Join(dir, "src", "up"))
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSandbox(sandbox.Sandbox{ID: "sbx-files001", Name: "files", ProjectPath: dir}); err != nil {
		t.Fatal(err)
	}
	return app, outside
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
	if strings.Join(names, ",") != "node_modules,src,big.txt,data.bin,key-link,logo.svg,pic.png,pipe,README.md,src-link" {
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
	for _, name := range []string{"../" + filepath.Base(outside), outside, "src/up/" + filepath.Base(filepath.Dir(outside)), "", ".", "src"} {
		if file := read(name); !strings.HasPrefix(file.Kind, "error") && file.Kind != "other" {
			t.Fatalf("%q must not be read: %+v", name, file)
		}
		if file := read(name); strings.Contains(file.Text, "synthetic-private-key") {
			t.Fatalf("%q leaked a file outside the project", name)
		}
	}
}
