package service

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	_ "golang.org/x/image/webp"
	"golang.org/x/sys/unix"

	"github.com/shaoxier/AIofCage/internal/sandbox"
)

// The terminal's file panel reads the project on the Mac, where it has the same path as in the
// sandbox, so it also works while the sandbox is stopped. The agent controls everything in the
// project, so reads stay inside it (os.Root refuses links and ".." that lead out), links and
// special files (FIFOs, devices) are never opened, and nothing here runs git or opens files with
// other programs. Paths and contents are not written to the audit log.

const (
	projectListLimit    = 2000
	projectScanLimit    = 20000 // entries read from one folder, hidden ones included
	projectPreviewLimit = 512 << 10
	projectLineLimit    = 2000        // bytes of one line shown
	projectPixelLimit   = 4096 * 4096 // a small file can declare a picture far too big to decode
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
	Clipped   bool      `json:"clipped,omitempty"` // lines longer than projectLineLimit were cut
}

var errOutsideProject = errors.New("路径必须在项目目录之内")

// projectPath turns a "/"-separated path from the interface into one relative to the project root.
func projectPath(p string) (string, error) {
	if p == "" || p == "." {
		return ".", nil
	}
	if strings.ContainsRune(p, 0) || !filepath.IsLocal(filepath.FromSlash(p)) {
		return "", errOutsideProject
	}
	return filepath.Clean(filepath.FromSlash(p)), nil
}

func (a *App) openProject(id string) (*os.Root, error) {
	sbx, err := a.store.Sandbox(id)
	if err != nil {
		return nil, err
	}
	workspace, err := filepath.EvalSymlinks(sandbox.Workspace())
	if err != nil {
		return nil, errors.New("工作区 ~/CageWorkspace 不存在")
	}
	return openProjectRoot(workspace, sbx.ProjectPath)
}

// openProjectRoot opens the project one folder at a time below the workspace, refusing links and
// anything that is not a folder at every step, and binds the root to the folder it opened. A
// project inside another sandbox's project can be swapped by that sandbox's agent for a link, which
// os.OpenRoot would follow, or for a FIFO, on which it would block.
func openProjectRoot(workspace, project string) (*os.Root, error) {
	rel, err := filepath.Rel(workspace, project)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, errors.New("项目目录必须位于 ~/CageWorkspace 之内")
	}
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	fd, err := unix.Open(workspace, flags, 0)
	if err != nil {
		return nil, errors.New("无法打开工作区")
	}
	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		next, err := unix.Openat(fd, name, flags, 0)
		unix.Close(fd)
		if err != nil {
			return nil, errors.New("项目目录不是普通文件夹，可能被替换成了链接")
		}
		fd = next
	}
	defer unix.Close(fd)
	// Opening /dev/fd/N duplicates the descriptor, so renaming the path later changes nothing.
	root, err := os.OpenRoot("/dev/fd/" + strconv.Itoa(fd))
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
	// O_DIRECTORY and O_NONBLOCK: a folder swapped for a FIFO after the check above fails at once
	// instead of blocking.
	folder, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ProjectListing{}, errors.New("无法读取这个文件夹")
	}
	defer folder.Close()
	listing := ProjectListing{Path: filepath.ToSlash(rel), Entries: []ProjectEntry{}}
	if listing.Path == "." {
		listing.Path = ""
	}
	// Folders such as node_modules can hold many thousands of entries; read a bounded number.
	for scanned := 0; !listing.More; {
		batch, err := folder.ReadDir(256)
		if err != nil && !errors.Is(err, io.EOF) {
			return ProjectListing{}, errors.New("无法读取这个文件夹")
		}
		for _, entry := range batch {
			if strings.HasPrefix(entry.Name(), ".") && !hidden {
				listing.Hidden++
				continue
			}
			if len(listing.Entries) == projectListLimit {
				listing.More = true
				break
			}
			item := ProjectEntry{Name: entry.Name(), Path: path.Join(listing.Path, entry.Name()), Kind: entryKind(entry.Type())}
			if info, err := entry.Info(); err == nil {
				item.Kind, item.Size, item.Modified = entryKind(info.Mode()), info.Size(), info.ModTime()
			}
			listing.Entries = append(listing.Entries, item)
		}
		if scanned += len(batch); len(batch) == 0 || err != nil {
			break
		}
		if scanned >= projectScanLimit {
			listing.More = true
		}
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
		// The page decodes the picture inside the app; a few KB can claim a gigapixel image.
		config, _, err := image.DecodeConfig(bytes.NewReader(data))
		if truncated || err != nil || config.Width*config.Height > projectPixelLimit {
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
	file.Kind, file.Truncated = "text", truncated
	file.Text, file.Clipped = clipLines(string(text))
	return file, nil
}

// clipLines cuts lines longer than projectLineLimit (minified code, data dumps), which the page
// would otherwise lay out as one enormous line.
func clipLines(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	clipped := false
	for i, line := range lines {
		if len(line) <= projectLineLimit {
			continue
		}
		cut := projectLineLimit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		lines[i], clipped = line[:cut]+" …", true
	}
	if !clipped {
		return text, false
	}
	return strings.Join(lines, "\n"), true
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
