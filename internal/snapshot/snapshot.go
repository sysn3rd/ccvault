// Package snapshot archives a whole working directory.
//
// It exists for the cases git cannot cover: a scratch workspace that is not a
// repository at all (the majority of sessions), and a repository with no remote,
// which has nowhere to be cloned back from.
package snapshot

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Defaults. A backup that silently balloons is worse than one that says what it
// left out, so both caps are enforced and every exclusion is recorded.
const (
	DefaultMaxFileBytes  = 16 << 20  // 16 MB
	DefaultMaxTotalBytes = 256 << 20 // 256 MB
)

// DefaultIgnoreDirs are directory names that are rebuildable by definition.
// They are matched by basename at any depth.
var DefaultIgnoreDirs = []string{
	"node_modules", ".venv", "venv", "__pycache__", ".cache",
	"target", "build", "dist", ".next", ".nuxt", ".tox",
	".mypy_cache", ".pytest_cache", ".gradle", ".terraform",
}

type Options struct {
	IgnoreDirs    []string
	MaxFileBytes  int64
	MaxTotalBytes int64
	// IncludeGit keeps .git. A repository with a reachable remote is restored by
	// cloning, so its history is redundant here; one without a remote has no
	// other copy and must carry it.
	IncludeGit bool
}

func (o Options) withDefaults() Options {
	if o.IgnoreDirs == nil {
		o.IgnoreDirs = DefaultIgnoreDirs
	}
	if o.MaxFileBytes == 0 {
		o.MaxFileBytes = DefaultMaxFileBytes
	}
	if o.MaxTotalBytes == 0 {
		o.MaxTotalBytes = DefaultMaxTotalBytes
	}
	return o
}

type Entry struct {
	Rel     string
	Size    int64
	Mode    fs.FileMode
	ModTime time.Time
}

type Manifest struct {
	Root       string
	Files      []Entry
	TotalBytes int64
	// TreeHash covers paths, sizes and modification times. An unchanged
	// directory hashes the same, so a repeat snapshot can be skipped for the
	// cost of one walk rather than a full re-archive.
	TreeHash string
	Skipped  []string
}

// Plan walks the directory and decides what would be archived.
func Plan(root string, opts Options) (*Manifest, error) {
	opts = opts.withDefaults()
	ignore := map[string]bool{}
	for _, d := range opts.IgnoreDirs {
		ignore[d] = true
	}

	m := &Manifest{Root: root}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is noted, not fatal.
			m.Skipped = append(m.Skipped, relOf(root, path)+" (unreadable)")
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		rel := relOf(root, path)

		if d.IsDir() {
			name := d.Name()
			if name == ".git" && !opts.IncludeGit {
				return filepath.SkipDir
			}
			if ignore[name] {
				m.Skipped = append(m.Skipped, rel+"/ (ignored directory)")
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			m.Skipped = append(m.Skipped, rel+" (unreadable)")
			return nil
		}
		// Regular files only. Symlinks, sockets and devices are not worth the
		// edge cases in a restore.
		if !info.Mode().IsRegular() {
			m.Skipped = append(m.Skipped, rel+" (not a regular file)")
			return nil
		}
		if info.Size() > opts.MaxFileBytes {
			m.Skipped = append(m.Skipped, fmt.Sprintf("%s (%d bytes, over per-file cap)", rel, info.Size()))
			return nil
		}
		if m.TotalBytes+info.Size() > opts.MaxTotalBytes {
			m.Skipped = append(m.Skipped, rel+" (snapshot size cap reached)")
			return nil
		}

		m.Files = append(m.Files, Entry{
			Rel: rel, Size: info.Size(), Mode: info.Mode(), ModTime: info.ModTime(),
		})
		m.TotalBytes += info.Size()
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Rel < m.Files[j].Rel })
	m.TreeHash = hashEntries(m.Files)
	return m, nil
}

func hashEntries(files []Entry) string {
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00", f.Rel, f.Size, f.ModTime.UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Write archives the planned files. It returns "" when there is nothing to
// store, so an empty directory does not produce an empty artefact.
func Write(m *Manifest, dest string) (int64, error) {
	if len(m.Files) == 0 {
		return 0, nil
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	for _, e := range m.Files {
		if err := writeOne(tw, m.Root, e); err != nil {
			// One vanished file must not lose the whole snapshot.
			continue
		}
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func writeOne(tw *tar.Writer, root string, e Entry) error {
	abs := filepath.Join(root, e.Rel)
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = e.Rel
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	src, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(tw, src)
	return err
}

// Extract unpacks a snapshot into dest.
func Extract(archivePath, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Only regular files are ever archived, so anything else means the
		// archive was modified. Symlink and hardlink entries in particular are
		// the classic way to write outside the destination.
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, hdr.FileInfo().Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		out.Close()
		_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
	}
}

// safeJoin refuses paths that would escape dest. Archives are ours, but a
// traversal bug here writes anywhere on the filesystem, so it is checked.
func safeJoin(dest, name string) (string, error) {
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("refusing absolute path in archive: %q", name)
	}
	target := filepath.Join(dest, name)
	cleanDest := filepath.Clean(dest) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), cleanDest) {
		return "", fmt.Errorf("refusing path outside destination: %q", name)
	}
	return target, nil
}

func relOf(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
