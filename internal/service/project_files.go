package service

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// The terminal's file panel reads the project on the Mac, where it has the same path as in the
// sandbox, so it also works while the sandbox is stopped. The agent controls everything in the
// project, so reads stay inside it (os.Root refuses links and ".." that lead out), links and
// special files (FIFOs, devices) are never opened, and nothing here runs git or opens files with
// other programs. Paths and contents are not written to the audit log.

const (
	projectListLimit    = 2000
	projectPreviewLimit = 512 << 10
)

type ProjectEntry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"` // relative to the project, "/"-separated
	Kind     string    `json:"kind"` // dir, file, link, other
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type ProjectListing struct {
	Path    string         `json:"path"`
	Entries []ProjectEntry `json:"entries"`
	// More is set when the folder has more entries than are listed.
	More   bool `json:"more"`
	Hidden int  `json:"hidden"` // entries starting with "." left out
}

type ProjectFile struct {
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	Modified  time.Time `json:"modified"`
	Kind      string    `json:"kind"` // text, image, binary, large, link, other
	Text      string    `json:"text,omitempty"`
	Image     string    `json:"image,omitempty"` // data: URL of a PNG, JPEG, GIF or WebP picture
	Truncated bool      `json:"truncated,omitempty"`
}

var errOutsideProject = errors.New("路径必须在项目目录之内")

// projectPath turns a "/"-separated path from the interface into one relative to the project root.
func projectPath(p string) (string, error) {
	if p == "" || p == "." {
		return ".", nil
	}
	if strings.ContainsAny(p, "\x00\\") || !filepath.IsLocal(filepath.FromSlash(p)) {
		return "", errOutsideProject
	}
	return filepath.Clean(filepath.FromSlash(p)), nil
}

func (a *App) openProject(id string) (*os.Root, error) {
	sbx, err := a.store.Sandbox(id)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(sbx.ProjectPath)
	if err != nil {
		return nil, errors.New("无法打开项目目录")
	}
	return root, nil
}

func entryKind(mode fs.FileMode) string {
	switch {
	case mode.IsDir():
		return "dir"
	case mode&fs.ModeSymlink != 0:
		return "link"
	case mode.IsRegular():
		return "file"
	}
	return "other"
}

// ListProjectEntries lists one folder of a sandbox's project: folders first, then files, by name.
func (a *App) ListProjectEntries(id, dir string, hidden bool) (ProjectListing, error) {
	rel, err := projectPath(dir)
	if err != nil {
		return ProjectListing{}, err
	}
	root, err := a.openProject(id)
	if err != nil {
		return ProjectListing{}, err
	}
	defer root.Close()
	// A link to a folder is shown as a link, not entered: it may lead somewhere else in the project
	// and make the tree endless.
	if info, err := root.Lstat(rel); err != nil || !info.IsDir() {
		return ProjectListing{}, errors.New("这个文件夹已经不存在")
	}
	folder, err := root.Open(rel)
	if err != nil {
		return ProjectListing{}, errors.New("无法读取这个文件夹")
	}
	defer folder.Close()
	// Folders such as node_modules can hold many thousands of entries; read a bounded number.
	entries, err := folder.ReadDir(projectListLimit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return ProjectListing{}, errors.New("无法读取这个文件夹")
	}
	listing := ProjectListing{Path: filepath.ToSlash(rel), Entries: []ProjectEntry{}}
	if listing.Path == "." {
		listing.Path = ""
	}
	if len(entries) > projectListLimit {
		entries, listing.More = entries[:projectListLimit], true
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") && !hidden {
			listing.Hidden++
			continue
		}
		item := ProjectEntry{Name: entry.Name(), Path: path.Join(listing.Path, entry.Name()), Kind: entryKind(entry.Type())}
		if info, err := entry.Info(); err == nil {
			item.Kind, item.Size, item.Modified = entryKind(info.Mode()), info.Size(), info.ModTime()
		}
		listing.Entries = append(listing.Entries, item)
	}
	slices.SortFunc(listing.Entries, func(x, y ProjectEntry) int {
		if (x.Kind == "dir") != (y.Kind == "dir") {
			if x.Kind == "dir" {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(x.Name), strings.ToLower(y.Name))
	})
	return listing, nil
}

// ReadProjectFile returns a preview of a regular file in a sandbox's project: up to 512 KiB of
// UTF-8 text, or a small picture. The interface shows text as plain text only.
func (a *App) ReadProjectFile(id, name string) (ProjectFile, error) {
	rel, err := projectPath(name)
	if err != nil || rel == "." {
		return ProjectFile{}, errOutsideProject
	}
	root, err := a.openProject(id)
	if err != nil {
		return ProjectFile{}, err
	}
	defer root.Close()
	file := ProjectFile{Path: filepath.ToSlash(rel)}
	info, err := root.Lstat(rel)
	if err != nil {
		return ProjectFile{}, errors.New("这个文件已经不存在")
	}
	file.Size, file.Modified = info.Size(), info.ModTime()
	if !info.Mode().IsRegular() {
		file.Kind = map[bool]string{true: "link", false: "other"}[info.Mode()&fs.ModeSymlink != 0]
		return file, nil
	}
	// Non-blocking, so a file swapped for a FIFO after the check above cannot hang the app; the
	// opened file is checked again.
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ProjectFile{}, errors.New("无法读取这个文件")
	}
	defer f.Close()
	if opened, err := f.Stat(); err != nil || !opened.Mode().IsRegular() {
		file.Kind = "other"
		return file, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, projectPreviewLimit+1))
	if err != nil {
		return ProjectFile{}, errors.New("无法读取这个文件")
	}
	truncated := len(data) > projectPreviewLimit
	if truncated {
		data = data[:projectPreviewLimit]
	}
	if kind := pictureType(data); kind != "" {
		if truncated {
			file.Kind = "large"
			return file, nil
		}
		file.Kind, file.Image = "image", "data:"+kind+";base64,"+base64.StdEncoding.EncodeToString(data)
		return file, nil
	}
	text := data
	if truncated {
		// Do not cut the last character in half.
		for i := 0; i < utf8.UTFMax && len(text) > 0 && !utf8.Valid(text); i++ {
			text = text[:len(text)-1]
		}
	}
	if bytes.IndexByte(text, 0) >= 0 || !utf8.Valid(text) {
		file.Kind = "binary"
		return file, nil
	}
	file.Kind, file.Text, file.Truncated = "text", string(text), truncated
	return file, nil
}

// pictureType recognises the raster formats the preview shows. SVG is text, never a picture: it
// can carry scripts.
func pictureType(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}
