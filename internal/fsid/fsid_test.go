package fsid

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRevalidateRejectsRestoredMtimeAndMetadataChanges(t *testing.T) {
	for _, operation := range []string{"rewrite", "permissions", "hardlink"} {
		t.Run(operation, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "valuable.bin")
			mustWrite(t, p, "original")
			id, fi, err := Of(p)
			if err != nil {
				t.Fatal(err)
			}
			if id.CtimeNs == 0 {
				t.Fatal("change time was not captured")
			}
			switch operation {
			case "rewrite":
				mustWrite(t, p, "replaced")
				if err := os.Chtimes(p, fi.ModTime(), fi.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(p, 0o600); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(p, p+"-link"); err != nil {
					t.Fatal(err)
				}
			}
			if err := Revalidate(p, id); !errors.Is(err, ErrChanged) {
				t.Fatalf("%s was accepted: %v", operation, err)
			}
		})
	}
}

type statInfo struct {
	os.FileInfo
	st syscall.Stat_t
}

func (s statInfo) Sys() any { return &s.st }

func TestCheckRejectsChangedLinkCount(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file")
	mustWrite(t, p, "hello")
	id, fi, err := Of(p)
	if err != nil {
		t.Fatal(err)
	}
	st := *fi.Sys().(*syscall.Stat_t)
	st.Nlink++
	if err := Check(statInfo{FileInfo: fi, st: st}, id); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed link count was accepted: %v", err)
	}
}

func TestFromOpenRetainsObjectIdentityAfterRename(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file")
	mustWrite(t, p, "hello")
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	want, _, err := FromOpen(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p, p+"-held"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, p, "other")
	got, _, err := FromOpen(f)
	if err != nil || !got.SameFile(want) {
		t.Fatalf("descriptor changed identity: %+v error=%v", got, err)
	}
}

func TestRevalidate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.bin")
	mustWrite(t, p, "hello")
	old := time.Date(2023, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	id, _, err := Of(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := Revalidate(p, id); err != nil {
		t.Fatalf("unchanged file failed revalidation: %v", err)
	}

	t.Run("mtime", func(t *testing.T) {
		newer := old.Add(time.Hour)
		if err := os.Chtimes(p, newer, newer); err != nil {
			t.Fatal(err)
		}
		defer os.Chtimes(p, old, old)
		if err := Revalidate(p, id); !errors.Is(err, ErrChanged) {
			t.Fatalf("want ErrChanged, got %v", err)
		}
	})

	t.Run("size", func(t *testing.T) {
		mustWrite(t, p, "hello world")
		os.Chtimes(p, old, old)
		if err := Revalidate(p, id); !errors.Is(err, ErrChanged) {
			t.Fatalf("want ErrChanged, got %v", err)
		}
		mustWrite(t, p, "hello")
		os.Chtimes(p, old, old)
	})

	t.Run("replaced", func(t *testing.T) {
		tmp := filepath.Join(dir, "other")
		mustWrite(t, tmp, "hello")
		os.Chtimes(tmp, old, old)
		if err := os.Rename(tmp, p); err != nil {
			t.Fatal(err)
		}
		if err := Revalidate(p, id); !errors.Is(err, ErrChanged) {
			t.Fatalf("want ErrChanged for replaced inode, got %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		id2, _, _ := Of(p)
		link := filepath.Join(dir, "link")
		if err := os.Symlink(p, link); err != nil {
			t.Fatal(err)
		}
		if err := Revalidate(link, id2); !errors.Is(err, ErrChanged) {
			t.Fatalf("want ErrChanged for symlink, got %v", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		if err := Revalidate(filepath.Join(dir, "nope"), id); !errors.Is(err, ErrChanged) {
			t.Fatalf("want ErrChanged for missing file, got %v", err)
		}
	})
}
