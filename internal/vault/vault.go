// Package vault stores ccvault's own copies of session data.
//
// Files are written owner-only: transcripts contain full file contents, command
// output and anything else that passed through a session.
package vault

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CopyTranscript copies a transcript into the vault, replacing any previous
// copy atomically. Transcripts are append-only and small (megabytes), so a whole
// -file copy is simpler and safer than a delta and costs little.
func CopyTranscript(src, dstDir, uuid string) (string, error) {
	dst := filepath.Join(dstDir, uuid+".jsonl")
	if err := copyFileAtomic(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// WritePatch stores a `git diff HEAD` for later replay. Returns "" when there is
// nothing to store, so callers can leave the column empty.
func WritePatch(dir, uuid string, at time.Time, patch string) (string, error) {
	if patch == "" {
		return "", nil
	}
	// git refuses a patch whose final line has no newline ("corrupt patch"),
	// so guarantee one regardless of how the caller obtained the text.
	if !strings.HasSuffix(patch, "\n") {
		patch += "\n"
	}
	name := fmt.Sprintf("%s-%d.patch", uuid, at.Unix())
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(patch), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}
