package sandbox

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"buildgate/internal/composeservices"
)

// materializeComposeBindInputs copies every relative bind source present in
// the immutable base commit into destination and returns services with each
// bind whose source the base commit lacks (a data directory such as
// ./pgdata, which Docker would otherwise create empty on the host) turned
// into an empty per-run named volume. The returned directory is empty when
// no service has a bind mount. destination is cleared first: it is per run,
// and a previous phase or activity attempt may have left it behind.
func materializeComposeBindInputs(services []composeservices.ServiceSpec, gitDir, baseSHA, destination string) ([]composeservices.ServiceSpec, string, error) {
	sources := map[string]bool{}
	for _, service := range services {
		for _, volume := range service.NamedVolumes {
			if volume.Type == "bind" {
				sources[volume.Source] = true
			}
		}
	}
	if len(sources) == 0 {
		return services, "", nil
	}
	if gitDir == "" || baseSHA == "" {
		return nil, "", errors.New("relative bind mounts require the base repository and commit provenance")
	}
	if !filepath.IsAbs(destination) {
		return nil, "", fmt.Errorf("bind-input destination %q is not absolute", destination)
	}
	if err := os.RemoveAll(destination); err != nil {
		return nil, "", fmt.Errorf("clear bind-input destination: %w", err)
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return nil, "", fmt.Errorf("create bind-input destination: %w", err)
	}

	names := make([]string, 0, len(sources))
	for source := range sources {
		names = append(names, source)
	}
	sort.Strings(names)
	missing := map[string]bool{}
	for _, source := range names {
		present, err := composeBindSourceInCommit(gitDir, baseSHA, source)
		if err != nil {
			return nil, "", err
		}
		if !present {
			missing[source] = true
			continue
		}
		if err := materializeComposeBindSource(gitDir, baseSHA, source, destination); err != nil {
			return nil, "", err
		}
	}

	out := make([]composeservices.ServiceSpec, len(services))
	for i, service := range services {
		volumes := make([]composeservices.VolumeMount, len(service.NamedVolumes))
		for j, volume := range service.NamedVolumes {
			if volume.Type == "bind" && missing[volume.Source] {
				volume = composeservices.VolumeMount{Type: "volume", Target: volume.Target, ReadOnly: volume.ReadOnly}
			}
			volumes[j] = volume
		}
		service.NamedVolumes = volumes
		out[i] = service
	}
	return out, destination, nil
}

// composeBindSourceInCommit reports whether baseSHA's tree has an entry at
// source. --literal-pathspecs: a source is a path, never a glob.
func composeBindSourceInCommit(gitDir, baseSHA, source string) (bool, error) {
	if !safeComposeBindSource(source) {
		return false, fmt.Errorf("bind source %q is not a safe repository-relative path", source)
	}
	out, err := exec.Command("git", "--literal-pathspecs", "-C", gitDir, "ls-tree", "-z", "--name-only", baseSHA, "--", source).Output()
	if err != nil {
		return false, fmt.Errorf("look up bind source %q in base commit %s: %w", source, baseSHA, err)
	}
	return len(out) > 0, nil
}

func materializeComposeBindSource(gitDir, baseSHA, source, destination string) error {
	if !safeComposeBindSource(source) {
		return fmt.Errorf("bind source %q is not a safe repository-relative path", source)
	}
	archiveBytes, err := exec.Command("git", "--literal-pathspecs", "-C", gitDir, "archive", "--format=tar", baseSHA, "--", source).Output()
	if err != nil {
		return fmt.Errorf("read bind source %q from base commit %s: %w", source, baseSHA, err)
	}

	stage, err := os.MkdirTemp(destination, ".stage-")
	if err != nil {
		return fmt.Errorf("create bind source staging directory: %w", err)
	}
	defer os.RemoveAll(stage)

	reader := tar.NewReader(bytes.NewReader(archiveBytes))
	found := false
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return fmt.Errorf("read archived bind source %q: %w", source, nextErr)
		}
		if header.Typeflag == tar.TypeXGlobalHeader || header.Typeflag == tar.TypeXHeader || header.Name == "pax_global_header" {
			continue
		}
		name, err := safeTarEntryName(header.Name)
		if err != nil {
			return fmt.Errorf("archive for bind source %q contains unsafe entry: %w", source, err)
		}
		if header.Typeflag == tar.TypeDir && strings.HasPrefix(source, name+"/") {
			// git archive lists each parent directory of a nested source
			// (db/ for db/migrations); only the source's own tree is copied.
			continue
		}
		if name != source && !strings.HasPrefix(name, source+"/") {
			return fmt.Errorf("archive for bind source %q contains unrelated entry %q", source, name)
		}
		target := filepath.Join(stage, filepath.FromSlash(name))
		if err := ensureContainedPath(stage, target); err != nil {
			return fmt.Errorf("archive for bind source %q: %w", source, err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return fmt.Errorf("create archived directory %q: %w", name, err)
			}
			if err := os.Chmod(target, os.FileMode(header.Mode)&0o777); err != nil {
				return fmt.Errorf("set archived directory mode %q: %w", name, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return fmt.Errorf("create archived file parent %q: %w", name, err)
			}
			file, createErr := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(header.Mode)&0o777)
			if createErr != nil {
				return fmt.Errorf("create archived file %q: %w", name, createErr)
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("copy archived file %q: %w", name, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close archived file %q: %w", name, closeErr)
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("archive for bind source %q contains a link at %q", source, name)
		default:
			return fmt.Errorf("archive for bind source %q contains unsupported special file %q", source, name)
		}
		if name == source {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("base commit %s does not contain bind source %q", baseSHA, source)
	}

	stagedSource := filepath.Join(stage, filepath.FromSlash(source))
	info, err := os.Lstat(stagedSource)
	if err != nil {
		return fmt.Errorf("stat materialized bind source %q: %w", source, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Errorf("materialized bind source %q is not a regular file or directory", source)
	}
	finalSource := filepath.Join(destination, filepath.FromSlash(source))
	if err := ensureContainedPath(destination, finalSource); err != nil {
		return fmt.Errorf("materialized bind source %q: %w", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(finalSource), 0o750); err != nil {
		return fmt.Errorf("create materialized bind source parent: %w", err)
	}
	if _, err := os.Lstat(finalSource); err == nil {
		return fmt.Errorf("materialized bind source %q already exists", source)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check materialized bind source %q: %w", source, err)
	}
	if err := os.Rename(stagedSource, finalSource); err != nil {
		return fmt.Errorf("materialize bind source %q: %w", source, err)
	}
	return nil
}

func safeComposeBindSource(source string) bool {
	if source == "" || source == "." || filepath.IsAbs(source) {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(source))
	if clean != source || strings.HasPrefix(clean, "../") || clean == ".." {
		return false
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." || part == ".git" {
			return false
		}
	}
	return true
}

func safeTarEntryName(name string) (string, error) {
	name = filepath.ToSlash(name)
	if name == "" || strings.HasPrefix(name, "/") {
		return "", errors.New("absolute or empty path")
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q escapes archive root", name)
	}
	return clean, nil
}

func ensureContainedPath(root, candidate string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("path %q escapes %q", candidate, root)
	}
	return nil
}
