package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// The git calls of a review-instruction snapshot: the environment and argv
// every host read of a trusted commit runs under, the writer that caps what a
// call may print, and the one `git cat-file --batch` process a plan reads its
// blobs through.

// reviewGitEnv is the environment of every git call here: no pathspec magic
// (a name is a literal), no lock, prompt, system or user configuration, no
// replace refs and no lazy fetch. Every GIT_ variable of the caller's
// environment is dropped first (GIT_DIR, GIT_WORK_TREE, GIT_OBJECT_DIRECTORY,
// GIT_CONFIG_*, GIT_REPLACE_REF_BASE and the rest redirect or alter a read), so
// only what is set here reaches git.
func reviewGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_LITERAL_PATHSPECS=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1")
}

// reviewGitArgs is the argv after "git" for running git in dir with nothing
// from any configuration that executes a program or changes what is read.
func reviewGitArgs(dir string, args ...string) []string {
	return append([]string{
		"--no-pager",
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "core.pager=cat", "-c", "diff.external=",
		"-c", "core.attributesFile=/dev/null",
		"-c", "protocol.allow=never",
		"-C", dir,
	}, args...)
}

// cappedWriter keeps the first max bytes written and counts the rest.
type cappedWriter struct {
	max  int
	buf  bytes.Buffer
	over int64
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	room := min(max(w.max-w.buf.Len(), 0), len(p))
	w.buf.Write(p[:room])
	w.over += int64(len(p) - room)
	return len(p), nil
}

// HardenedGitCommand is the git command every host read of a trusted commit
// runs: git in dir with args, under reviewGitArgs and reviewGitEnv, so no
// GIT_ variable of the caller, replace ref, configuration or lazy fetch
// changes which objects are read. The caller sets the output and runs it.
func HardenedGitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", reviewGitArgs(dir, args...)...)
	cmd.Env = reviewGitEnv()
	return cmd
}

func reviewGit(ctx context.Context, dir string, stdout io.Writer, args ...string) error {
	cmd := HardenedGitCommand(ctx, dir, args...)
	stderr := &cappedWriter{max: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.buf.String()))
	}
	return nil
}

var fullGitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// blobReader is one `git cat-file --batch` process that serves every blob a
// snapshot reads: a request is an object id on its input, the answer a line
// "<id> blob <size>" and then the bytes. The size is known before a byte of
// the body is read, so a limit is applied without reading past it. After any
// error the reader is closed and answers nothing more: every such error ends
// the snapshot.
type blobReader struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *cappedWriter
	oids   *regexp.Regexp // the object ids a request may name
	err    error          // why the reader stopped
}

func startBlobReader(ctx context.Context, root string) (*blobReader, error) {
	cmd := HardenedGitCommand(ctx, root, "cat-file", "--batch")
	b := &blobReader{cmd: cmd, stderr: &cappedWriter{max: 4096}, oids: fullGitSHAPattern}
	cmd.Stderr = b.stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	b.in, b.out = in, bufio.NewReaderSize(out, 64<<10)
	return b, nil
}

// close ends the process. It is safe to call twice.
func (b *blobReader) close() {
	if b == nil || b.cmd == nil {
		return
	}
	_ = b.in.Close()
	_ = b.cmd.Process.Kill()
	_ = b.cmd.Wait()
	b.cmd = nil
	if b.err == nil {
		b.err = errors.New("git cat-file: the reader is closed")
	}
}

// fail closes the reader and returns err, which every later request returns.
func (b *blobReader) fail(err error) error {
	b.err = err
	b.close()
	return err
}

// size asks for the blob oid and returns its size; the body follows on b.out.
func (b *blobReader) size(oid string) (int64, error) {
	if b.err != nil {
		return 0, b.err
	}
	if !b.oids.MatchString(oid) {
		return 0, b.fail(fmt.Errorf("git cat-file: %q is not an object id", oid))
	}
	if _, err := io.WriteString(b.in, oid+"\n"); err != nil {
		return 0, b.fail(fmt.Errorf("git cat-file: %w: %s", err, strings.TrimSpace(b.stderr.buf.String())))
	}
	line, err := b.out.ReadSlice('\n')
	if err != nil {
		return 0, b.fail(fmt.Errorf("git cat-file: %w", err))
	}
	rest, ok := strings.CutPrefix(strings.TrimSuffix(string(line), "\n"), oid+" blob ")
	size, perr := strconv.ParseInt(rest, 10, 64)
	if !ok || perr != nil || size < 0 {
		return 0, b.fail(fmt.Errorf("git cat-file: object %s is not a blob (%s)", oid, strings.TrimSpace(string(line))))
	}
	return size, nil
}

// body copies the size bytes that follow a size answer to w.
func (b *blobReader) body(w io.Writer, size int64) error {
	if _, err := io.CopyN(w, b.out, size); err != nil {
		return b.fail(fmt.Errorf("git cat-file: %w", err))
	}
	if c, err := b.out.ReadByte(); err != nil || c != '\n' {
		return b.fail(fmt.Errorf("git cat-file: a blob of %d bytes did not end where its size says", size))
	}
	return nil
}

// blobs is the plan's reader, started on the first read.
func (s *planState) blobs(ctx context.Context, root string) (*blobReader, error) {
	if s.reader == nil {
		b, err := startBlobReader(ctx, root)
		if err != nil {
			return nil, fmt.Errorf("review instructions: %w", err)
		}
		s.reader = b
	}
	return s.reader, nil
}

// closeBlobs ends the plan's reader, if one was started.
func (s *planState) closeBlobs() {
	if s != nil {
		s.reader.close()
	}
}

// readBlob reads one blob by object id, at most limit bytes.
func (s *planState) readBlob(ctx context.Context, root, oid string, limit int) ([]byte, error) {
	b, err := s.blobs(ctx, root)
	if err != nil {
		return nil, err
	}
	size, err := b.size(oid)
	if err != nil {
		return nil, fmt.Errorf("review instructions: %w", err)
	}
	if size > int64(limit) {
		_ = b.fail(fmt.Errorf("git cat-file: blob %s is over %d bytes", oid, limit))
		return nil, fmt.Errorf("review instructions: blob %s is over %d bytes", oid, limit)
	}
	var out bytes.Buffer
	if err := b.body(&out, size); err != nil {
		return nil, fmt.Errorf("review instructions: %w", err)
	}
	return out.Bytes(), nil
}
