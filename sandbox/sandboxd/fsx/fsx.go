// Package fsx implements the fs_* tools: line-numbered reads with binary
// detection, atomic writes that keep mode and ownership, exact-match edits,
// listing, ripgrep search, stat, move, and delete-to-trash.
package fsx

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/textx"
)

// Limits.
const (
	MaxReadBytes   = 2 << 20
	DefaultLimit   = 2000
	MaxLimit       = 20000
	MaxLineChars   = 4000
	MaxWriteBytes  = 10 << 20
	DefaultListMax = 1000
	MaxListEntries = 10000
	TrashDirName   = ".headmesh-trash"
	stRDONLY       = 0x1 // ST_RDONLY in statfs f_flags
	mountinfoPath  = "/proc/self/mountinfo"
)

// FS carries the policy for the fs tools.
type FS struct {
	// Home resolves relative paths.
	Home string
	// WritableRoots are the read-write mounts, used for error messages and
	// to choose the trash location.
	WritableRoots []string
	// FallbackTrash is used when no writable root contains the path.
	FallbackTrash string
	Now           func() time.Time
}

func (f *FS) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Resolve makes p absolute (relative to Home) and clean.
func (f *FS) Resolve(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("path is required")
	}
	if strings.ContainsRune(p, 0) {
		return "", errors.New("path contains a NUL byte")
	}
	if strings.HasPrefix(p, "~/") || p == "~" {
		p = filepath.Join(f.Home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(f.Home, p)
	}
	return filepath.Clean(p), nil
}

// ReadOnlyError explains a refused write.
type ReadOnlyError struct {
	Path       string
	MountPoint string
	Writable   []string
}

func (e *ReadOnlyError) Error() string {
	msg := fmt.Sprintf("%s is on a read-only mount (%s)", e.Path, e.MountPoint)
	if len(e.Writable) > 0 {
		msg += "; writable mounts: " + strings.Join(e.Writable, ", ")
	}
	return msg
}

// checkWritable refuses paths whose filesystem is mounted read-only.
func (f *FS) checkWritable(p string) error {
	dir := p
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", dir, err)
	}
	if st.Flags&stRDONLY != 0 {
		return &ReadOnlyError{Path: p, MountPoint: MountPoint(dir), Writable: f.WritableRoots}
	}
	return nil
}

// MountPoint returns the longest mount point containing p, from mountinfo.
func MountPoint(p string) string {
	data, err := os.ReadFile(mountinfoPath)
	if err != nil {
		return "/"
	}
	best := "/"
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMount(fields[4])
		if (p == mp || strings.HasPrefix(p, strings.TrimSuffix(mp, "/")+"/")) && len(mp) > len(best) {
			best = mp
		}
	}
	return best
}

func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ReadResult is the fs_read output.
type ReadResult struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	TotalLines int    `json:"total_lines,omitempty"`
	Size       int64  `json:"size"`
	Binary     bool   `json:"binary,omitempty"`
	Truncated  bool   `json:"truncated"`
	NextOffset int    `json:"next_offset,omitempty"`
	More       string `json:"more,omitempty"`
}

// Read returns lines [offset, offset+limit) numbered like `cat -n`.
func (f *FS) Read(path string, offset, limit int) (ReadResult, error) {
	p, err := f.Resolve(path)
	if err != nil {
		return ReadResult{}, err
	}
	fh, err := os.Open(p) //nolint:gosec // G304: reading agent-chosen paths is the tool
	if err != nil {
		return ReadResult{}, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = fh.Close() }()
	fi, err := fh.Stat()
	if err != nil {
		return ReadResult{}, fmt.Errorf("stat: %w", err)
	}
	if fi.IsDir() {
		return ReadResult{}, fmt.Errorf("%s is a directory; use fs_list", p)
	}
	res := ReadResult{Path: p, Size: fi.Size()}
	br := bufio.NewReaderSize(fh, 64<<10)
	sample, _ := br.Peek(8192)
	if textx.LooksBinary(sample) {
		res.Binary = true
		res.More = fmt.Sprintf("binary file (%d bytes); use exec (xxd, file, base64) or artifact_put path=%s", fi.Size(), p)
		return res, nil
	}
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	var out bytes.Buffer
	line := 0
	eof := false
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			line++
			if line >= offset {
				if line >= offset+limit || out.Len() >= MaxReadBytes {
					res.Truncated = true
					break
				}
				text := strings.TrimRight(string(raw), "\r\n")
				if len(text) > MaxLineChars {
					text = string(textx.CutUTF8([]byte(text), MaxLineChars)) + " … [line truncated]"
				}
				fmt.Fprintf(&out, "%6d\t%s\n", line, text)
				if res.StartLine == 0 {
					res.StartLine = line
				}
				res.EndLine = line
			}
		}
		if errors.Is(err, io.EOF) {
			eof = true
			break
		}
		if err != nil {
			return res, fmt.Errorf("read: %w", err)
		}
	}
	res.Content = out.String()
	if eof {
		res.TotalLines = line
	}
	if res.Truncated {
		res.NextOffset = res.EndLine + 1
		res.More = fmt.Sprintf("more lines follow; call fs_read with offset=%d", res.NextOffset)
	}
	if res.StartLine == 0 && line < offset && line > 0 {
		res.More = fmt.Sprintf("offset %d is past the end (file has %d lines)", offset, line)
	}
	return res, nil
}

// WriteResult is the fs_write and fs_edit output.
type WriteResult struct {
	Path         string `json:"path"`
	Bytes        int    `json:"bytes"`
	SHA256       string `json:"sha256"`
	Created      bool   `json:"created"`
	Replacements int    `json:"replacements,omitempty"`
}

// Write replaces path atomically with content (text, or base64 when
// encoding is "base64"). mode is octal like "0755"; empty keeps the existing
// mode or uses 0644.
func (f *FS) Write(path, content, encoding, mode string, createDirs bool) (WriteResult, error) {
	p, err := f.Resolve(path)
	if err != nil {
		return WriteResult{}, err
	}
	data := []byte(content)
	switch encoding {
	case "", "utf-8", "utf8", "text":
	case "base64":
		if data, err = base64.StdEncoding.DecodeString(content); err != nil {
			return WriteResult{}, fmt.Errorf("content is not valid base64: %w", err)
		}
	default:
		return WriteResult{}, fmt.Errorf("unknown encoding %q (utf-8 or base64)", encoding)
	}
	if len(data) > MaxWriteBytes {
		return WriteResult{}, fmt.Errorf("content is %d bytes; fs_write is limited to %d", len(data), MaxWriteBytes)
	}
	var perm os.FileMode
	if mode != "" {
		v, err := strconv.ParseUint(mode, 8, 32)
		if err != nil || v > 0o7777 {
			return WriteResult{}, fmt.Errorf("mode %q must be octal like 0644", mode)
		}
		perm = os.FileMode(v)
	}
	return f.writeAtomic(p, data, perm, createDirs)
}

func (f *FS) writeAtomic(p string, data []byte, perm os.FileMode, createDirs bool) (WriteResult, error) {
	// Write through symlinks to their target, as an editor would.
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	if err := f.checkWritable(p); err != nil {
		return WriteResult{}, err
	}
	dir := filepath.Dir(p)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if !createDirs {
			return WriteResult{}, fmt.Errorf("directory %s does not exist (set create_dirs=true)", dir)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: project directories, like mkdir -p
			return WriteResult{}, fmt.Errorf("create dirs: %w", err)
		}
	}
	created := true
	uid, gid := -1, -1
	if fi, err := os.Stat(p); err == nil {
		if fi.IsDir() {
			return WriteResult{}, fmt.Errorf("%s is a directory", p)
		}
		created = false
		if perm == 0 {
			perm = fi.Mode().Perm() | (fi.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky))
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}
	if perm == 0 {
		perm = 0o644
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(p)+".hm-*")
	if err != nil {
		return WriteResult{}, fmt.Errorf("temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return WriteResult{}, fmt.Errorf("write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return WriteResult{}, fmt.Errorf("sync: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return WriteResult{}, fmt.Errorf("chmod: %w", err)
	}
	if uid >= 0 && (uid != os.Getuid() || gid != os.Getgid()) {
		_ = tmp.Chown(uid, gid) // best effort; only root can give files away
	}
	if err := tmp.Close(); err != nil {
		return WriteResult{}, fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return WriteResult{}, fmt.Errorf("replace %s: %w", p, err)
	}
	sum := sha256.Sum256(data)
	return WriteResult{Path: p, Bytes: len(data), SHA256: hex.EncodeToString(sum[:]), Created: created}, nil
}

// Edit replaces old with new exactly. Without replaceAll, old must occur
// exactly once.
func (f *FS) Edit(path, old, new string, replaceAll bool) (WriteResult, error) {
	p, err := f.Resolve(path)
	if err != nil {
		return WriteResult{}, err
	}
	if old == "" {
		return WriteResult{}, errors.New("old_string must not be empty")
	}
	if old == new {
		return WriteResult{}, errors.New("old_string and new_string are identical")
	}
	data, err := os.ReadFile(p) //nolint:gosec // G304: editing agent-chosen paths is the tool
	if err != nil {
		return WriteResult{}, fmt.Errorf("read: %w", err)
	}
	n := bytes.Count(data, []byte(old))
	switch {
	case n == 0:
		return WriteResult{}, fmt.Errorf("old_string not found in %s (it must match exactly, including whitespace)", p)
	case n > 1 && !replaceAll:
		return WriteResult{}, fmt.Errorf("old_string occurs %d times in %s; add surrounding context to make it unique or set replace_all=true", n, p)
	}
	limit := 1
	if replaceAll {
		limit = -1
	}
	out := bytes.Replace(data, []byte(old), []byte(new), limit)
	res, err := f.writeAtomic(p, out, 0, false)
	if err != nil {
		return res, err
	}
	res.Replacements = 1
	if replaceAll {
		res.Replacements = n
	}
	return res, nil
}

// Entry is one fs_list item.
type Entry struct {
	Path   string    `json:"path"`
	Type   string    `json:"type"`
	Size   int64     `json:"size"`
	Mode   string    `json:"mode"`
	MTime  time.Time `json:"mtime"`
	Target string    `json:"target,omitempty"`
}

// ListResult is the fs_list output.
type ListResult struct {
	Path      string  `json:"path"`
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated"`
	More      string  `json:"more,omitempty"`
}

func typeOf(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "dir"
	case m&fs.ModeSymlink != 0:
		return "symlink"
	case m.IsRegular():
		return "file"
	default:
		return "other"
	}
}

// List walks path to depth, filtering by glob (matched against the base
// name, or the relative path when it contains '/').
func (f *FS) List(path string, depth int, glob string, includeHidden bool, limit int) (ListResult, error) {
	root, err := f.Resolve(path)
	if err != nil {
		return ListResult{}, err
	}
	fi, err := os.Stat(root)
	if err != nil {
		return ListResult{}, fmt.Errorf("stat: %w", err)
	}
	if !fi.IsDir() {
		return ListResult{}, fmt.Errorf("%s is not a directory", root)
	}
	if depth <= 0 {
		depth = 1
	}
	depth = min(depth, 10)
	if limit <= 0 {
		limit = DefaultListMax
	}
	limit = min(limit, MaxListEntries)
	if glob != "" {
		if _, err := filepath.Match(glob, "x"); err != nil {
			return ListResult{}, fmt.Errorf("bad glob %q: %w", glob, err)
		}
	}
	res := ListResult{Path: root, Entries: []Entry{}}
	errStop := errors.New("stop")
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if p == root {
			return nil
		}
		if err != nil {
			return nil //nolint:nilerr // unreadable entries are skipped, not fatal
		}
		rel, _ := filepath.Rel(root, p)
		lvl := strings.Count(rel, string(filepath.Separator)) + 1
		name := d.Name()
		if !includeHidden && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		match := true
		if glob != "" {
			target := name
			if strings.Contains(glob, "/") {
				target = rel
			}
			match, _ = filepath.Match(glob, target)
		}
		if match {
			if len(res.Entries) >= limit {
				res.Truncated = true
				return errStop
			}
			info, err := d.Info()
			if err == nil {
				e := Entry{Path: p, Type: typeOf(info.Mode()), Size: info.Size(), Mode: fmt.Sprintf("%04o", info.Mode().Perm()), MTime: info.ModTime().UTC()}
				if e.Type == "symlink" {
					e.Target, _ = os.Readlink(p)
				}
				res.Entries = append(res.Entries, e)
			}
		}
		if d.IsDir() && (lvl >= depth || name == ".git" || name == TrashDirName) {
			return filepath.SkipDir
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errStop) {
		return res, fmt.Errorf("walk: %w", walkErr)
	}
	if res.Truncated {
		res.More = fmt.Sprintf("stopped at %d entries; narrow with glob or depth, or raise limit (max %d)", limit, MaxListEntries)
	}
	return res, nil
}

// StatResult is the fs_stat output.
type StatResult struct {
	Path          string    `json:"path"`
	Exists        bool      `json:"exists"`
	Type          string    `json:"type,omitempty"`
	Size          int64     `json:"size,omitempty"`
	Mode          string    `json:"mode,omitempty"`
	UID           int       `json:"uid,omitempty"`
	GID           int       `json:"gid,omitempty"`
	MTime         time.Time `json:"mtime,omitzero"`
	SymlinkTarget string    `json:"symlink_target,omitempty"`
	MountPoint    string    `json:"mount_point,omitempty"`
	ReadOnlyMount bool      `json:"readonly_mount"`
	Writable      bool      `json:"writable"`
}

// Stat describes path without following a final symlink.
func (f *FS) Stat(path string) (StatResult, error) {
	p, err := f.Resolve(path)
	if err != nil {
		return StatResult{}, err
	}
	res := StatResult{Path: p}
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("stat: %w", err)
	}
	res.Exists = true
	res.Type, res.Size, res.Mode, res.MTime = typeOf(fi.Mode()), fi.Size(), fmt.Sprintf("%04o", fi.Mode().Perm()), fi.ModTime().UTC()
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		res.UID, res.GID = int(st.Uid), int(st.Gid)
	}
	if res.Type == "symlink" {
		res.SymlinkTarget, _ = os.Readlink(p)
	}
	res.MountPoint = MountPoint(p)
	var sfs syscall.Statfs_t
	if syscall.Statfs(p, &sfs) == nil {
		res.ReadOnlyMount = sfs.Flags&stRDONLY != 0
	}
	res.Writable = !res.ReadOnlyMount && syscall.Access(p, 2) == nil
	return res, nil
}

// MoveResult is the fs_move output.
type MoveResult struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Move renames src to dst within one filesystem.
func (f *FS) Move(src, dst string, overwrite bool) (MoveResult, error) {
	s, err := f.Resolve(src)
	if err != nil {
		return MoveResult{}, err
	}
	d, err := f.Resolve(dst)
	if err != nil {
		return MoveResult{}, err
	}
	if _, err := os.Lstat(s); err != nil {
		return MoveResult{}, fmt.Errorf("source: %w", err)
	}
	if fi, err := os.Stat(d); err == nil && fi.IsDir() {
		d = filepath.Join(d, filepath.Base(s))
	}
	if _, err := os.Lstat(d); err == nil && !overwrite {
		return MoveResult{}, fmt.Errorf("%s exists (set overwrite=true to replace it)", d)
	}
	for _, p := range []string{s, d} {
		if err := f.checkWritable(p); err != nil {
			return MoveResult{}, err
		}
	}
	if err := os.Rename(s, d); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return MoveResult{}, fmt.Errorf("%s and %s are on different filesystems; use exec with mv", s, d)
		}
		return MoveResult{}, fmt.Errorf("move: %w", err)
	}
	return MoveResult{From: s, To: d}, nil
}

// DeleteResult is the fs_delete output.
type DeleteResult struct {
	Path      string `json:"path"`
	TrashedTo string `json:"trashed_to"`
	Restore   string `json:"restore"`
}

// Delete moves path into <root>/.headmesh-trash/<timestamp>/<relative path>.
func (f *FS) Delete(path string, recursive bool) (DeleteResult, error) {
	p, err := f.Resolve(path)
	if err != nil {
		return DeleteResult{}, err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return DeleteResult{}, fmt.Errorf("stat: %w", err)
	}
	if p == "/" || slices.Contains(f.WritableRoots, p) || p == f.Home {
		return DeleteResult{}, fmt.Errorf("refusing to delete mount root %s", p)
	}
	if fi.IsDir() && !recursive {
		entries, err := os.ReadDir(p)
		if err != nil {
			return DeleteResult{}, fmt.Errorf("read dir: %w", err)
		}
		if len(entries) > 0 {
			return DeleteResult{}, fmt.Errorf("%s is a non-empty directory (set recursive=true)", p)
		}
	}
	if err := f.checkWritable(p); err != nil {
		return DeleteResult{}, err
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	stamp := f.now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b)
	var candidates []string
	if root := longestRoot(f.WritableRoots, p); root != "" {
		candidates = append(candidates, root)
	}
	if mp := MountPoint(p); mp != "/" {
		candidates = append(candidates, mp)
	}
	if f.FallbackTrash != "" {
		candidates = append(candidates, f.FallbackTrash)
	}
	var lastErr error
	for _, root := range candidates {
		var dest string
		if root == f.FallbackTrash {
			dest = filepath.Join(root, stamp, strings.TrimPrefix(p, "/"))
		} else {
			rel, err := filepath.Rel(root, p)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			dest = filepath.Join(root, TrashDirName, stamp, rel)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil { //nolint:gosec // G301: trash mirrors project directories
			lastErr = err
			continue
		}
		if err := os.Rename(p, dest); err != nil {
			lastErr = err
			continue
		}
		return DeleteResult{Path: p, TrashedTo: dest, Restore: fmt.Sprintf("fs_move src=%s dst=%s", dest, p)}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no trash location on the same filesystem")
	}
	return DeleteResult{}, fmt.Errorf("could not move %s to the trash: %w (use exec with rm if you really mean it)", p, lastErr)
}

func longestRoot(roots []string, p string) string {
	best := ""
	for _, r := range roots {
		if (p == r || strings.HasPrefix(p, strings.TrimSuffix(r, "/")+"/")) && len(r) > len(best) {
			best = r
		}
	}
	return best
}
