package sandbox

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
	"time"
)

// InstallCaptureLimits bound what CaptureInstallTree writes.
type InstallCaptureLimits struct {
	MaxBytes   int64
	MaxEntries int
}

// DefaultInstallCaptureLimits are 4 GiB of file content and 500,000 entries.
var DefaultInstallCaptureLimits = InstallCaptureLimits{MaxBytes: 4 << 30, MaxEntries: 500_000}

// ErrInstallCaptureRefused wraps every refusal of CaptureInstallTree: an
// entry it does not carry, or a limit passed.
var ErrInstallCaptureRefused = errors.New("install capture refused")

// CaptureInstallTree writes, as a tar stream to w, each of paths (relative to
// dir) that exists, and returns those it wrote. It reads dir only through an
// os.Root, so nothing outside dir is read, and it carries only regular files,
// directories and relative symlinks that, resolved as the kernel will through
// every other link of the tree (resolve), never leave the cache path: any
// other entry (a device, a socket, a FIFO, an absolute link, a link or chain
// of links that leaves the path) refuses the capture, naming it.
// Every entry is owned 0:0 with a normal mode: directories 0755, files 0755
// when any execute bit was set, else 0644; set-id and sticky bits are gone.
// Modification times are zero, so the tar is the same for the same tree.
func CaptureInstallTree(dir string, paths []string, w io.Writer, limits InstallCaptureLimits) ([]string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	c := &installCapture{root: root, tw: tar.NewWriter(w), limits: limits}
	var captured []string
	for _, p := range paths {
		info, err := root.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := c.parents(p); err != nil {
			return nil, err
		}
		if err := c.walk(p, p, info); err != nil {
			return nil, err
		}
		captured = append(captured, p)
	}
	return captured, c.tw.Close()
}

type installCapture struct {
	root    *os.Root
	tw      *tar.Writer
	limits  InstallCaptureLimits
	bytes   int64
	entries int
	dirs    map[string]bool
}

func (c *installCapture) refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInstallCaptureRefused, fmt.Sprintf(format, args...))
}

// parents writes the directories above a nested cache path (a/b for a/b/c),
// owned 0:0 and mode 0755, once each.
func (c *installCapture) parents(p string) error {
	if c.dirs == nil {
		c.dirs = map[string]bool{}
	}
	var chain []string
	for d := path.Dir(p); d != "."; d = path.Dir(d) {
		chain = append(chain, d)
	}
	slices.Reverse(chain)
	for _, d := range chain {
		if c.dirs[d] {
			continue
		}
		c.dirs[d] = true
		if err := c.header(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o755}); err != nil {
			return err
		}
	}
	return nil
}

func (c *installCapture) header(h *tar.Header) error {
	if c.entries++; c.entries > c.limits.MaxEntries {
		return c.refuse("more than %d entries", c.limits.MaxEntries)
	}
	h.Uid, h.Gid, h.Uname, h.Gname, h.ModTime, h.Format = 0, 0, "", "", time.Unix(0, 0), tar.FormatPAX
	return c.tw.WriteHeader(h)
}

// walk writes rel, which is under the cache path top, and what it holds.
func (c *installCapture) walk(top, rel string, info fs.FileInfo) error {
	switch info.Mode().Type() {
	case fs.ModeDir:
		if err := c.header(&tar.Header{Typeflag: tar.TypeDir, Name: rel + "/", Mode: 0o755}); err != nil {
			return err
		}
		c.dirs[rel] = true
		return c.walkDir(top, rel)
	case 0:
		return c.file(rel, info, normalFileMode(info.Mode()))
	case fs.ModeSymlink:
		if rel == top {
			return c.refuse("the cache path %s is itself a link", rel)
		}
		target, err := c.root.Readlink(rel)
		if err != nil {
			return err
		}
		if _, err := c.resolve(top, path.Dir(rel), target, 0); err != nil && !errors.Is(err, errDangling) {
			return c.refuse("%s is a link to %q, which %v", rel, target, err)
		}
		return c.header(&tar.Header{Typeflag: tar.TypeSymlink, Name: rel, Linkname: target, Mode: 0o777})
	default:
		return c.refuse("%s is not a regular file, directory or link (%s)", rel, info.Mode().Type())
	}
}

// normalFileMode is a captured file's mode: 0755 when any execute bit is
// set, else 0644. The worker restoring it owns the copy, so it can always
// write and remove it.
func normalFileMode(m fs.FileMode) int64 {
	if m.Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// errDangling ends a resolution at a name the tree does not have: the
// kernel's resolution fails there, so nothing past it is reached.
var errDangling = errors.New("names a path the tree does not have")

// maxLinkDepth bounds how many links one resolution follows, as the kernel's
// own limit does.
const maxLinkDepth = 40

// resolve resolves target from the directory dir (a real directory of the
// tree, never reached through a link) the way the kernel will once the tree
// is restored: component by component, following every link it meets in the
// tree (so a chain such as a/sub -> .. with a/esc -> sub/../.. is resolved as
// a chain, not as text), and fails when any step leaves top, the target or a
// link on the way is absolute or empty, or the chain is too deep. A name that
// does not exist ends the resolution where it is: the kernel stops there too.
func (c *installCapture) resolve(top, dir, target string, depth int) (string, error) {
	if depth > maxLinkDepth {
		return "", errors.New("is a chain of more than 40 links")
	}
	if target == "" || path.IsAbs(target) {
		return "", errors.New("is empty or absolute")
	}
	cur := dir
	for _, comp := range strings.Split(target, "/") {
		switch comp {
		case "", ".":
			continue
		case "..":
			if cur == top {
				return "", fmt.Errorf("leaves the cache path %s", top)
			}
			cur = path.Dir(cur)
			continue
		}
		next := cur + "/" + comp
		info, err := c.root.Lstat(next)
		if err != nil {
			return next, errDangling
		}
		if info.Mode().Type() != fs.ModeSymlink {
			cur = next
			continue
		}
		link, err := c.root.Readlink(next)
		if err != nil {
			return "", err
		}
		if cur, err = c.resolve(top, cur, link, depth+1); err != nil {
			return cur, err
		}
	}
	return cur, nil
}

// dirBatch is how many entries a directory read takes at a time; the entry
// cap is checked after each batch, so a huge directory is refused before it
// is read whole.
const dirBatch = 1024

// readDirNames reads the names of the directory rel in batches, refusing it
// as soon as the names read so far would pass the entry cap.
func (c *installCapture) readDirNames(rel string) ([]string, error) {
	f, err := c.root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var names []string
	for {
		batch, err := f.ReadDir(dirBatch)
		for _, e := range batch {
			names = append(names, e.Name())
		}
		if c.entries+len(names) > c.limits.MaxEntries {
			return nil, c.refuse("more than %d entries", c.limits.MaxEntries)
		}
		if errors.Is(err, io.EOF) || (err == nil && len(batch) == 0) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (c *installCapture) walkDir(top, rel string) error {
	names, err := c.readDirNames(rel)
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		child := rel + "/" + name
		info, err := c.root.Lstat(child)
		if err != nil {
			return err
		}
		if err := c.walk(top, child, info); err != nil {
			return err
		}
	}
	return nil
}

func (c *installCapture) file(rel string, info fs.FileInfo, mode int64) error {
	if c.bytes += info.Size(); c.bytes > c.limits.MaxBytes {
		return c.refuse("more than %d bytes", c.limits.MaxBytes)
	}
	f, err := c.root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(opened, info) {
		return c.refuse("%s changed while it was captured", rel)
	}
	if err := c.header(&tar.Header{Typeflag: tar.TypeReg, Name: rel, Mode: mode, Size: info.Size()}); err != nil {
		return err
	}
	if _, err := io.CopyN(c.tw, f, info.Size()); err != nil {
		return fmt.Errorf("capture %s: %w", rel, err)
	}
	return nil
}
