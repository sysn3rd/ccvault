package vault

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Untracked bundling limits. Untracked files respect .gitignore (git is asked
// with --exclude-standard), so this is normally a handful of scratch files —
// but a session in a directory with a thin .gitignore could sweep in something
// enormous, and a backup tool must never silently balloon.
const (
	MaxUntrackedFileBytes  = 8 << 20   // 8 MB per file
	MaxUntrackedTotalBytes = 128 << 20 // 128 MB per bundle
)

type UntrackedResult struct {
	Path    string // "" when there was nothing to store
	Bytes   int64  // size of the bundle on disk
	Stored  []string
	Skipped []string // recorded, never silently dropped
}

// WriteUntracked archives files git does not know about.
//
// `git diff HEAD` covers modified tracked files only, so without this an
// untracked file present during a session is unrecoverable once the directory
// is deleted — the same one-shot loss as an uncaptured commit SHA.
func WriteUntracked(dir, uuid, worktreeRoot string, at time.Time, files []string) (*UntrackedResult, error) {
	res := &UntrackedResult{}
	if len(files) == 0 || worktreeRoot == "" {
		return res, nil
	}

	path := filepath.Join(dir, fmt.Sprintf("%s-%d.untracked.tar.gz", uuid, at.Unix()))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return res, err
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	var total int64
	for _, rel := range files {
		if rel == "" {
			continue
		}
		abs := filepath.Join(worktreeRoot, rel)
		info, err := os.Lstat(abs)
		if err != nil {
			res.Skipped = append(res.Skipped, rel+" (unreadable)")
			continue
		}
		// Regular files only: a symlink or socket is not worth the edge cases.
		if !info.Mode().IsRegular() {
			res.Skipped = append(res.Skipped, rel+" (not a regular file)")
			continue
		}
		if info.Size() > MaxUntrackedFileBytes {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s (%d bytes, over per-file cap)", rel, info.Size()))
			continue
		}
		if total+info.Size() > MaxUntrackedTotalBytes {
			res.Skipped = append(res.Skipped, rel+" (bundle size cap reached)")
			continue
		}

		if err := copyInto(tw, abs, rel, info); err != nil {
			res.Skipped = append(res.Skipped, rel+" ("+err.Error()+")")
			continue
		}
		total += info.Size()
		res.Stored = append(res.Stored, rel)
	}

	if err := tw.Close(); err != nil {
		return res, err
	}
	if err := gz.Close(); err != nil {
		return res, err
	}

	if len(res.Stored) == 0 {
		f.Close()
		os.Remove(path)
		return res, nil
	}
	if st, err := f.Stat(); err == nil {
		res.Bytes = st.Size()
	}
	res.Path = path
	return res, nil
}

func copyInto(tw *tar.Writer, abs, rel string, info os.FileInfo) error {
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = rel
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
