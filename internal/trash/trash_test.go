package trash

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"spacehound/internal/fsid"
	"spacehound/internal/macosfs"
	"spacehound/internal/oplog"
)

var stamp = time.Date(2026, 3, 4, 5, 6, 7, 0, time.Local)

func setup(t *testing.T) (src, trashDir string, tr *Trasher) {
	t.Setenv("TMPDIR", "/private/tmp")
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	trashDir = filepath.Join(root, "Trash")
	os.Mkdir(trashDir, 0o700)
	src = filepath.Join(root, "work", "report.pdf")
	os.MkdirAll(filepath.Dir(src), 0o755)
	if err := os.WriteFile(src, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr = &Trasher{Journal: oplog.New(filepath.Join(root, "logs")),
		DirFor: func(string, uint64) (string, error) { return trashDir, nil },
		Now:    func() time.Time { return stamp },
	}
	return
}

func TestCandidateName(t *testing.T) {
	cases := []struct {
		base string
		n    int
		want string
	}{
		{"a.txt", 0, "a.txt"},
		{"a.txt", 1, "a (SpaceHound 2026-03-04 05.06.07).txt"},
		{"a.txt", 3, "a (SpaceHound 2026-03-04 05.06.07 3).txt"},
		{"archive.tar.gz", 1, "archive.tar (SpaceHound 2026-03-04 05.06.07).gz"},
		{".env", 1, ".env (SpaceHound 2026-03-04 05.06.07)"},
		{"noext", 2, "noext (SpaceHound 2026-03-04 05.06.07 2)"},
	}
	for _, c := range cases {
		if got := CandidateName(c.base, stamp, c.n); got != c.want {
			t.Errorf("CandidateName(%q,%d) = %q, want %q", c.base, c.n, got, c.want)
		}
	}
}

func TestTrashCollisionSafe(t *testing.T) {
	src, trashDir, tr := setup(t)
	// Existing items with the first two candidate names must not be overwritten.
	os.WriteFile(filepath.Join(trashDir, "report.pdf"), []byte("older 1"), 0o644)
	os.WriteFile(filepath.Join(trashDir, CandidateName("report.pdf", stamp, 1)), []byte("older 2"), 0o644)

	id, _, err := fsid.Of(src)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tr.Trash(src, id)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(trashDir, CandidateName("report.pdf", stamp, 2))
	if res.Dest != want {
		t.Errorf("dest = %s, want %s", res.Dest, want)
	}
	if _, err := os.Lstat(src); !os.IsNotExist(err) {
		t.Errorf("source still exists")
	}
	for name, content := range map[string]string{
		"report.pdf":                          "older 1",
		CandidateName("report.pdf", stamp, 1): "older 2",
		CandidateName("report.pdf", stamp, 2): "original",
	} {
		b, err := os.ReadFile(filepath.Join(trashDir, name))
		if err != nil || string(b) != content {
			t.Errorf("%s = %q (%v), want %q", name, b, err, content)
		}
	}
}

func TestTrashRefusesChangedFile(t *testing.T) {
	src, trashDir, tr := setup(t)
	id, _, _ := fsid.Of(src)
	os.WriteFile(src, []byte("modified contents"), 0o644)
	if _, err := tr.Trash(src, id); !errors.Is(err, fsid.ErrChanged) {
		t.Fatalf("want ErrChanged, got %v", err)
	}
	if _, err := os.Lstat(src); err != nil {
		t.Errorf("source must remain: %v", err)
	}
	entries, _ := os.ReadDir(trashDir)
	if len(entries) != 0 {
		t.Errorf("nothing should be in Trash, found %d", len(entries))
	}
}

func TestTrashUnavailableKeepsFile(t *testing.T) {
	src, _, tr := setup(t)
	tr.DirFor = func(string, uint64) (string, error) { return "", ErrNoTrash }
	id, _, _ := fsid.Of(src)
	if _, err := tr.Trash(src, id); !errors.Is(err, ErrNoTrash) {
		t.Fatalf("want ErrNoTrash, got %v", err)
	}
	if _, err := os.Lstat(src); err != nil {
		t.Errorf("file must never be deleted when Trash is unavailable: %v", err)
	}
}

func TestTrashRejectsRelativePath(t *testing.T) {
	_, _, tr := setup(t)
	if _, err := tr.Trash("relative/file", fsid.Identity{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestTrashNeverRestoresAReplacement(t *testing.T) {
	src, dir, tr := setup(t)
	id, _, _ := fsid.Of(src)
	tr.move = func(from *os.File, name string, to *os.File, dest string) error {
		if err := macosfs.Rename(from, name, to, dest); err != nil {
			return err
		}
		if err := macosfs.Rename(to, dest, to, "preserved-original"); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, dest), []byte("replacement"), 0o600)
	}
	res, err := tr.Trash(src, id)
	if err == nil || res.RecoveryPath != filepath.Join(dir, "preserved-original") {
		t.Fatalf("lost original recovery location: %+v %v", res, err)
	}
	for path, want := range map[string]string{res.RecoveryPath: "original", filepath.Join(dir, "report.pdf"): "replacement"} {
		got, readErr := os.ReadFile(path)
		if readErr != nil || string(got) != want {
			t.Fatalf("file moved or lost: %s = %q %v", path, got, readErr)
		}
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("restored unrelated replacement", err)
	}
	pending, err := tr.Journal.Pending()
	if err != nil || len(pending) != 1 || pending[0].RecoveryPath != res.RecoveryPath {
		t.Fatal("original missing from journal", pending, err)
	}
}

func TestTrashReportsRecoveryAfterDirectorySwap(t *testing.T) {
	src, dir, tr := setup(t)
	id, _, _ := fsid.Of(src)
	elsewhere := t.TempDir()
	calls := 0
	tr.move = func(from *os.File, name string, to *os.File, dest string) error {
		if err := macosfs.Rename(from, name, to, dest); err != nil {
			return err
		}
		calls++
		if calls == 1 {
			if err := os.Rename(dir, dir+"-moved"); err != nil {
				return err
			}
			return os.Symlink(elsewhere, dir)
		}
		return nil
	}
	res, err := tr.Trash(src, id)
	if err == nil || res.RecoveryPath != filepath.Join(dir+"-moved", "report.pdf") {
		t.Fatal("missing actual original location", res, err)
	}
	if got, err := os.ReadFile(res.RecoveryPath); err != nil || string(got) != "original" {
		t.Fatal("original not preserved", string(got), err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("unexpected name-based rollback", err)
	}
	if entries, err := os.ReadDir(elsewhere); err != nil || len(entries) != 0 {
		t.Fatal("wrote through symlink", entries, err)
	}
}

func TestUnsupportedExclusiveMoveNeverFallsBack(t *testing.T) {
	src, _, tr := setup(t)
	id, _, _ := fsid.Of(src)
	tr.move = func(*os.File, string, *os.File, string) error { return syscall.ENOTSUP }
	if _, err := tr.Trash(src, id); !errors.Is(err, syscall.ENOTSUP) {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(src); err != nil || string(got) != "original" {
		t.Fatal("source lost after unsupported move", string(got), err)
	}
}
