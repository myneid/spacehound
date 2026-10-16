//go:build darwin && cgo

package macosfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fixture(t *testing.T) (string, *os.File) {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := OpenDir(path, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dir.Close() })
	return path, dir
}

func file(t *testing.T, dir *os.File, name string, data []byte) *os.File {
	t.Helper()
	f, err := OpenAt(dir, name, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	return f
}

func command(t *testing.T, name string, args ...string) {
	t.Helper()
	if output, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture command %s: %v: %s", name, err, output)
	}
}

func fingerprint(t *testing.T, f *os.File) Fingerprint {
	t.Helper()
	fp, err := FingerprintFile(context.Background(), f, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestOpenDirRefusesSymlinkAncestors(t *testing.T) {
	path, dir := fixture(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(path, "redirect")); err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{false, true} {
		if f, err := OpenDir(filepath.Join(path, "redirect", "child"), create, 0o700); err == nil {
			f.Close()
			t.Fatalf("followed symlink with create=%v", create)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "child")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created through symlink: %v", err)
	}
	if f, err := OpenAt(dir, "redirect", syscall.O_RDONLY, 0); err == nil {
		f.Close()
		t.Fatal("opened symlink")
	}
	created := filepath.Join(path, "new", "nested")
	f, err := OpenDir(created, true, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, invalid := range []string{"", ".", "..", "a/b", "/absolute", "nul\x00name"} {
		if f, err := OpenAt(dir, invalid, syscall.O_RDONLY, 0); err == nil {
			f.Close()
			t.Errorf("accepted component %q", invalid)
		}
	}
}

func TestDirectoryDescriptorSurvivesPathReplacement(t *testing.T) {
	path, parent := fixture(t)
	if err := MkdirAt(parent, "original", 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenAt(parent, "original", syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := Rename(parent, "original", parent, "moved"); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(path, "original")); err != nil {
		t.Fatal(err)
	}
	f := file(t, dir, "held", []byte("correct directory"))
	if err := FullSync(f); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "held")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("followed replacement path: %v", err)
	}
	current, err := Path(f)
	if err != nil {
		t.Fatal(err)
	}
	if current != filepath.Join(path, "moved", "held") {
		t.Fatalf("stale descriptor path: %q", current)
	}
}

func TestExclusiveRenameNeverOverwrites(t *testing.T) {
	path, dir := fixture(t)
	file(t, dir, "source", []byte("source"))
	file(t, dir, "destination", []byte("keep"))
	if err := Rename(dir, "source", dir, "destination"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("want existing-destination refusal, got %v", err)
	}
	data, err := os.ReadFile(filepath.Join(path, "destination"))
	if err != nil || string(data) != "keep" {
		t.Fatalf("destination changed: %q, %v", data, err)
	}
	if err := os.Symlink("destination", filepath.Join(path, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := Rename(dir, "source", dir, "symlink"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("want symlink-destination refusal, got %v", err)
	}
	if err := Rename(dir, "source", dir, "published"); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteMetadataCopyAndVerification(t *testing.T) {
	path, dir := fixture(t)
	src := file(t, dir, "source", []byte("primary contents"))
	dst := file(t, dir, "destination", []byte("primary contents"))
	source := filepath.Join(path, "source")
	command(t, "/usr/bin/xattr", "-w", "spacehound.fixture", "extended contents", source)
	// A multi-chunk resource fork proves verification is bounded and complete.
	fork := bytes.Repeat([]byte("resource"), 350000)
	if err := os.WriteFile(source+"/..namedfork/rsrc", fork, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := src.Chmod(0o640); err != nil {
		t.Fatal(err)
	}
	command(t, "/bin/chmod", "+a", "everyone allow read,readattr,readextattr,readsecurity", source)
	old := time.Date(2021, 3, 4, 5, 6, 7, 123456789, time.UTC)
	if err := os.Chtimes(source, old, old); err != nil {
		t.Fatal(err)
	}
	want := fingerprint(t, src)
	if err := CopyMetadata(src, dst); err != nil {
		t.Fatal(err)
	}
	if got := fingerprint(t, dst); got != want {
		t.Fatalf("incomplete native copy: got %+v; want %+v", got, want)
	}
	if current := fingerprint(t, src); current != want {
		t.Fatal("native metadata copy altered source")
	}
	if _, err := src.Seek(3, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	_ = fingerprint(t, src)
	if offset, err := src.Seek(0, io.SeekCurrent); err != nil || offset != 3 {
		t.Fatalf("fingerprinting altered offset: %d, %v", offset, err)
	}
	command(t, "/usr/bin/xattr", "-w", "spacehound.fixture", "changed contents", source)
	if got := fingerprint(t, src); got.SHA256 != want.SHA256 || got.MetadataSHA256 == want.MetadataSHA256 {
		t.Fatal("attribute change was not separately detected")
	}
	withACL := fingerprint(t, src)
	command(t, "/bin/chmod", "-N", source)
	if got := fingerprint(t, src); got.MetadataSHA256 == withACL.MetadataSHA256 {
		t.Fatal("ACL removal was not detected")
	}
}

func TestMetadataPreservesBirthtimeDistinctFromMtime(t *testing.T) {
	path, dir := fixture(t)
	src := file(t, dir, "source", []byte("source"))
	dst := file(t, dir, "destination", []byte("source"))
	old := time.Date(2021, 3, 4, 5, 6, 7, 123456789, time.UTC)
	if err := os.Chtimes(filepath.Join(path, "source"), old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := src.WriteAt([]byte("source"), 0); err != nil {
		t.Fatal(err)
	}
	before, err := stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if before.Birthtimespec == before.Mtimespec {
		t.Fatal("fixture must have a creation time distinct from modification time")
	}
	if err := CopyMetadata(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, want := fingerprint(t, dst), fingerprint(t, src); got != want {
		t.Fatalf("birthtime or other metadata lost: got %+v; want %+v", got, want)
	}
}

func TestMakePrivateClearsACLAndRefusesHardLinks(t *testing.T) {
	path, dir := fixture(t)
	f := file(t, dir, "private", []byte("private contents"))
	name := filepath.Join(path, "private")
	command(t, "/bin/chmod", "+a", "everyone allow read,readattr,readextattr,readsecurity", name)
	if err := f.Chmod(0o666); err != nil {
		t.Fatal(err)
	}
	if err := MakePrivate(f, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private mode not enforced: %v, %v", info, err)
	}
	output, err := exec.Command("/bin/ls", "-le", name).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output, []byte("everyone")) {
		t.Fatalf("extended ACL survived: %s", output)
	}
	if err := linkAt(dir, "private", "alias"); err != nil {
		t.Fatal(err)
	}
	if err := MakePrivate(f, 0o600); err == nil {
		t.Fatal("made multiply-linked file private")
	}
	command(t, "/bin/chmod", "+a", "everyone allow list,search", path)
	if err := MakePrivate(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestMakePrivateDoesNotChangeAlreadyPrivateMetadata(t *testing.T) {
	_, dir := fixture(t)
	f := file(t, dir, "private", []byte("private contents"))
	for _, item := range []struct {
		file *os.File
		mode os.FileMode
	}{{f, 0o600}, {dir, 0o700}} {
		if err := MakePrivate(item.file, item.mode); err != nil {
			t.Fatal(err)
		}
		before, err := stat(item.file)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if err := MakePrivate(item.file, item.mode); err != nil {
				t.Fatal(err)
			}
		}
		after, err := stat(item.file)
		if err != nil || !stable(before, after) {
			t.Fatalf("private metadata changed: before %+v; after %+v; %v", before, after, err)
		}
	}
}

func TestMakePrivateClearsACLWhenModeAlreadyPrivate(t *testing.T) {
	path, dir := fixture(t)
	f := file(t, dir, "private", []byte("private contents"))
	name := filepath.Join(path, "private")
	command(t, "/bin/chmod", "+a", "everyone allow read,readattr,readextattr,readsecurity", name)
	if err := MakePrivate(f, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("/bin/ls", "-le", name).CombinedOutput()
	if err != nil || bytes.Contains(output, []byte("everyone")) {
		t.Fatalf("extended ACL survived private mode: %s, %v", output, err)
	}
}

func TestResourceForkChangeChangesFingerprint(t *testing.T) {
	path, dir := fixture(t)
	f := file(t, dir, "source", []byte("same primary contents"))
	source := filepath.Join(path, "source")
	if err := os.WriteFile(source+"/..namedfork/rsrc", []byte("fork one"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := fingerprint(t, f)
	if err := os.WriteFile(source+"/..namedfork/rsrc", []byte("fork two"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := fingerprint(t, f)
	if before.SHA256 != after.SHA256 || before.MetadataSHA256 == after.MetadataSHA256 {
		t.Fatal("resource fork was not verified as content")
	}
}

func TestFingerprintDetectsMutationAndCancellation(t *testing.T) {
	_, dir := fixture(t)
	f := file(t, dir, "source", bytes.Repeat([]byte("x"), 2*1024*1024))
	_, err := FingerprintFile(context.Background(), f, func(done int64) {
		if done == 1024*1024 {
			if _, err := f.WriteAt([]byte("changed"), 0); err != nil {
				t.Fatal(err)
			}
		}
	})
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("concurrent mutation accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FingerprintFile(ctx, f, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	if _, err := FingerprintFile(context.Background(), dir, nil); err == nil {
		t.Fatal("fingerprinted a directory")
	}
}

func TestGuardedUnlinkCapability(t *testing.T) {
	path, dir := fixture(t)
	err := CheckGuardedUnlink(dir)
	if err != nil && !errors.Is(err, ErrGuardedUnlinkUnsupported) {
		t.Fatalf("unexpected capability error: %v", err)
	}
	if err != nil {
		t.Logf("permanent removal must refuse on this filesystem: %v", err)
	} else {
		f := file(t, dir, "busy", []byte("fixture"))
		if err := Unlink(dir, "busy", true); !errors.Is(err, syscall.EBUSY) {
			t.Fatalf("busy file not protected: %v", err)
		}
		f.Close()
		if err := Unlink(dir, "busy", true); err != nil {
			t.Fatalf("closed file could not be removed: %v", err)
		}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".spacehound-probe-") {
			t.Errorf("capability probe left artifact %q", entry.Name())
		}
	}
}
