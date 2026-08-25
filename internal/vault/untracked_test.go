package vault

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func bundleContents(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = string(data)
	}
	return out
}

func TestWriteUntrackedStoresContents(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "notes.txt"), []byte("scratch notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "sub", "deep.txt"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := WriteUntracked(t.TempDir(), "uuid", work, time.Unix(1000, 0),
		[]string{"notes.txt", "sub/deep.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path == "" {
		t.Fatal("no bundle written")
	}
	got := bundleContents(t, res.Path)
	if got["notes.txt"] != "scratch notes" {
		t.Errorf("notes.txt = %q", got["notes.txt"])
	}
	if got["sub/deep.txt"] != "nested" {
		t.Errorf("sub/deep.txt = %q", got["sub/deep.txt"])
	}

	info, err := os.Stat(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("bundle mode = %o, want 600", perm)
	}
}

// Silent truncation would read as "everything was backed up" when it wasn't.
func TestWriteUntrackedRecordsSkips(t *testing.T) {
	work := t.TempDir()
	big := make([]byte, MaxUntrackedFileBytes+1)
	if err := os.WriteFile(filepath.Join(work, "huge.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "small.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := WriteUntracked(t.TempDir(), "uuid", work, time.Unix(1000, 0),
		[]string{"huge.bin", "small.txt", "vanished.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stored) != 1 || res.Stored[0] != "small.txt" {
		t.Errorf("Stored = %v, want [small.txt]", res.Stored)
	}
	if len(res.Skipped) != 2 {
		t.Errorf("Skipped = %v, want the oversized and the missing file", res.Skipped)
	}
}

func TestWriteUntrackedNoFilesWritesNothing(t *testing.T) {
	dir := t.TempDir()
	res, err := WriteUntracked(dir, "uuid", t.TempDir(), time.Unix(1000, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "" {
		t.Error("a bundle was written for zero untracked files")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("directory should be empty, has %d entries", len(entries))
	}
}
