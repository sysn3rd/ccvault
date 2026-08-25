package snapshot

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkfile(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPlanSkipsRebuildableDirectories(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "src/main.go", []byte("package main"))
	mkfile(t, root, "node_modules/left-pad/index.js", []byte("junk"))
	mkfile(t, root, "target/debug/binary", []byte("junk"))
	mkfile(t, root, "notes.md", []byte("keep"))

	m, err := Plan(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range m.Files {
		got = append(got, f.Rel)
	}
	want := []string{"notes.md", filepath.Join("src", "main.go")}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Files = %v, want %v", got, want)
	}
	if len(m.Skipped) != 2 {
		t.Errorf("Skipped = %v, want node_modules and target", m.Skipped)
	}
}

// A repo with a reachable remote is restored by cloning, so its history is
// redundant; one without a remote has no other copy of it.
func TestPlanGitInclusionIsOptional(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, ".git/HEAD", []byte("ref: refs/heads/main"))
	mkfile(t, root, "file.txt", []byte("x"))

	m, err := Plan(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 1 {
		t.Errorf("without IncludeGit: Files = %d, want 1", len(m.Files))
	}

	m, err = Plan(root, Options{IncludeGit: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 2 {
		t.Errorf("with IncludeGit: Files = %d, want 2", len(m.Files))
	}
}

// The tree hash is what makes repeat snapshots free for an idle directory.
func TestTreeHashDetectsChange(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "a.txt", []byte("one"))

	first, err := Plan(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	again, err := Plan(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if first.TreeHash != again.TreeHash {
		t.Error("unchanged directory produced a different tree hash")
	}

	mkfile(t, root, "b.txt", []byte("two"))
	changed, err := Plan(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if changed.TreeHash == first.TreeHash {
		t.Error("added file did not change the tree hash")
	}
}

func TestCapsAreRecordedNotSilent(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "big.bin", make([]byte, 2048))
	mkfile(t, root, "small.txt", []byte("ok"))

	m, err := Plan(root, Options{MaxFileBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 1 || m.Files[0].Rel != "small.txt" {
		t.Errorf("Files = %v, want just small.txt", m.Files)
	}
	if len(m.Skipped) != 1 || !strings.Contains(m.Skipped[0], "over per-file cap") {
		t.Errorf("Skipped = %v, want the oversized file recorded", m.Skipped)
	}
}

func TestWriteAndExtractRoundTrip(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "notes.md", []byte("hello"))
	mkfile(t, root, "sub/deep.txt", []byte("nested content"))

	m, err := Plan(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "snap.tar.gz")
	size, err := Write(m, archive)
	if err != nil {
		t.Fatal(err)
	}
	if size == 0 {
		t.Fatal("archive is empty")
	}
	if info, err := os.Stat(archive); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("archive mode = %o, want 600", info.Mode().Perm())
	}

	dest := t.TempDir()
	if err := Extract(archive, dest); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"notes.md":     "hello",
		"sub/deep.txt": "nested content",
	} {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

func TestWriteEmptyDirProducesNothing(t *testing.T) {
	m, err := Plan(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "snap.tar.gz")
	size, err := Write(m, archive)
	if err != nil {
		t.Fatal(err)
	}
	if size != 0 {
		t.Error("empty directory produced an archive")
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Error("archive file should not have been created")
	}
}

// Extraction writes to disk from an archive; a traversal path must never escape.
func TestSafeJoinRefusesTraversal(t *testing.T) {
	for _, bad := range []string{"../escape", "/etc/passwd", "sub/../../escape"} {
		if _, err := safeJoin("/tmp/dest", bad); err == nil {
			t.Errorf("safeJoin accepted %q", bad)
		}
	}
	if _, err := safeJoin("/tmp/dest", "sub/ok.txt"); err != nil {
		t.Errorf("safeJoin rejected a legitimate path: %v", err)
	}
}

// Only regular files are ever archived, so a symlink entry means the archive
// was modified. Writing it out is how an attacker escapes the destination.
func TestExtractIgnoresNonRegularEntries(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "tampered.tar.gz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	// A symlink pointing outside, and a legitimate file alongside it.
	if err := tw.WriteHeader(&tar.Header{
		Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "../../../../etc/passwd", Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	body := []byte("legitimate")
	if err := tw.WriteHeader(&tar.Header{
		Name: "real.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	f.Close()

	dest := t.TempDir()
	if err := Extract(archive, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "escape")); !os.IsNotExist(err) {
		t.Error("the symlink entry was written out")
	}
	got, err := os.ReadFile(filepath.Join(dest, "real.txt"))
	if err != nil || string(got) != "legitimate" {
		t.Errorf("the legitimate entry did not survive: %q %v", got, err)
	}
}
