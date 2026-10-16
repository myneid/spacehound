package offload

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

	"spacehound/internal/diskspace"
	"spacehound/internal/fsid"
	"spacehound/internal/macosfs"
	"spacehound/internal/oplog"
	"spacehound/internal/units"
	"spacehound/internal/volumes"
)

var day = time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

func TestDestPath(t *testing.T) {
	cases := []struct {
		src, want string
	}{
		{"/Users/me/Movies/a.mov", "/Volumes/X/SpaceHound Archive/host/2026-05-06/Movies/a.mov"},
		{"/opt/data/big.iso", "/Volumes/X/SpaceHound Archive/host/2026-05-06/_outside_home/opt/data/big.iso"},
	}
	for _, c := range cases {
		got, err := DestPath("/Volumes/X", "host", day, "/Users/me", c.src)
		if err != nil || got != c.want {
			t.Errorf("DestPath(%q) = %q, %v; want %q", c.src, got, err, c.want)
		}
	}
	for _, bad := range []string{"relative/a", "/Users/me/../../etc/passwd", "/Users/me/a/./b", "/Users/me//a", "/Users/me"} {
		if got, err := DestPath("/Volumes/X", "host", day, "/Users/me", bad); err == nil && bad != "/Users/me" {
			t.Errorf("DestPath(%q) accepted: %q", bad, got)
		}
	}
	if _, err := DestPath("/", "host", day, "/Users/me", "/Users/me/a"); err == nil {
		t.Error("root volume accepted")
	}
	// A hostile hostname cannot climb out of the archive.
	got, err := DestPath("/Volumes/X", "../../etc", day, "/Users/me", "/Users/me/a")
	if err != nil || !strings.HasPrefix(got, "/Volumes/X/SpaceHound Archive/") || strings.Contains(got, "/../") {
		t.Errorf("hostile host: %q %v", got, err)
	}
	for in, want := range map[string]string{"..": "Mac", "": "Mac", "a/b:c": "a_b_c", " mac ": "mac"} {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

type env struct {
	home, vol, src string
	id             fsid.Identity
	o              *Offloader
	req            Request
}

func setup(t *testing.T, content []byte) *env {
	t.Helper()
	t.Setenv("TMPDIR", "/private/tmp")
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	e := &env{home: filepath.Join(root, "home"), vol: filepath.Join(root, "Volumes", "Ext")}
	if err := os.MkdirAll(filepath.Join(e.home, "Movies"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.vol, 0755); err != nil {
		t.Fatal(err)
	}
	e.src = filepath.Join(e.home, "Movies", "film.mov")
	if err := os.WriteFile(e.src, content, 0644); err != nil {
		t.Fatal(err)
	}
	mt := day.Add(-48 * time.Hour)
	if err := os.Chtimes(e.src, mt, mt); err != nil {
		t.Fatal(err)
	}
	var err error
	e.id, _, err = fsid.Of(e.src)
	if err != nil {
		t.Fatal(err)
	}
	e.o = &Offloader{Home: e.home, Host: "testhost", Now: func() time.Time { return day }, Copy: CopyFile, AllowSameDevice: true, Journal: oplog.New(filepath.Join(root, "logs")), Statfs: func(p string) (diskspace.Info, error) {
		return diskspace.Info{MountPoint: p, FSType: "apfs", Flags: diskspace.FlagLocal, Free: 10 * units.GiB, Total: 20 * units.GiB}, nil
	}}
	e.req = Request{Source: e.src, Identity: e.id, Volume: volumes.Volume{Path: e.vol, Name: "Ext", Suitable: true, Writable: true, Free: 10 * units.GiB, Total: 20 * units.GiB, Dev: e.id.Dev}}
	return e
}
func data(n int) []byte { return bytes.Repeat([]byte("spacehound-"), n) }
func (e *env) refresh(t *testing.T) {
	t.Helper()
	var err error
	e.id, _, err = fsid.Of(e.src)
	if err != nil {
		t.Fatal(err)
	}
	e.req.Identity = e.id
}
func copied(t *testing.T, e *env) Result {
	t.Helper()
	res, err := e.o.Offload(context.Background(), e.req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Verified {
		t.Fatal("copy not verified")
	}
	return res
}
func retained(t *testing.T, e *env, content []byte) {
	t.Helper()
	got, err := os.ReadFile(e.src)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("source changed/lost: %v", err)
	}
}
func TestOffloadSuccessKeepsSource(t *testing.T) {
	e := setup(t, data(50000))
	res := copied(t, e)
	retained(t, e, data(50000))
	if res.Dest != filepath.Join(e.vol, ArchiveDir, "testhost", "2026-05-06", "Movies", "film.mov") {
		t.Fatal(res.Dest)
	}
	if b, err := os.ReadFile(res.Dest); err != nil || !bytes.Equal(b, data(50000)) {
		t.Fatal("copy differs", err)
	}
	if res.SHA256 == "" || res.Fingerprint.MetadataSHA256 == "" {
		t.Fatal("incomplete fingerprint")
	}
	if err := CheckRemovable(res); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSource(res); errors.Is(err, macosfs.ErrGuardedUnlinkUnsupported) {
		retained(t, e, data(50000))
	} else if err != nil {
		t.Fatal(err)
	} else if _, err := os.Lstat(e.src); !os.IsNotExist(err) {
		t.Fatal("source not removed", err)
	}
	pending, err := e.o.Journal.Pending()
	if err != nil || len(pending) != 0 {
		t.Fatal("unfinished operation", pending, err)
	}
}
func TestOffloadRefusesExistingDest(t *testing.T) {
	for _, kind := range []string{"file", "directory", "dangling-symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			e := setup(t, data(10))
			dest, err := e.o.Plan(e.req)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file":
				err = os.WriteFile(dest, []byte("precious"), 0o644)
			case "directory":
				err = os.Mkdir(dest, 0o755)
			case "dangling-symlink":
				err = os.Symlink("missing-target", dest)
			case "fifo":
				err = syscall.Mkfifo(dest, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			e.o.Copy = func(context.Context, *os.File, *os.File, func(int64)) error {
				t.Fatal("copied source for an occupied destination")
				return nil
			}
			res, err := e.o.Offload(context.Background(), e.req, nil)
			if !errors.Is(err, ErrDestExists) || res.Partial != "" || res.Verified {
				t.Fatal(res, err)
			}
			entries, err := os.ReadDir(filepath.Dir(dest))
			if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(dest) {
				t.Fatalf("unexpected copy artifacts: %v, %v", entries, err)
			}
			journal, err := os.ReadFile(e.o.Journal.Path())
			if err != nil || len(journal) != 0 {
				t.Fatalf("journaled an occupied destination: %s, %v", journal, err)
			}
			if kind == "file" {
				if b, err := os.ReadFile(dest); err != nil || string(b) != "precious" {
					t.Fatalf("destination changed: %q, %v", b, err)
				}
			}
			retained(t, e, data(10))
		})
	}
}
func TestOffloadRefusesDestAppearingDuringCopy(t *testing.T) {
	e := setup(t, data(10))
	dest, _ := e.o.Plan(e.req)
	e.o.Copy = func(ctx context.Context, src, dst *os.File, p func(int64)) error {
		if err := os.WriteFile(dest, []byte("raced"), 0644); err != nil {
			return err
		}
		return CopyFile(ctx, src, dst, p)
	}
	res, err := e.o.Offload(context.Background(), e.req, nil)
	if !errors.Is(err, ErrDestExists) || res.Verified || res.Partial == "" {
		t.Fatal(res, err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "raced" {
		t.Fatal("overwritten")
	}
}
func TestOffloadRefusesSymlinkInDestination(t *testing.T) {
	e := setup(t, data(10))
	elsewhere := t.TempDir()
	os.Symlink(elsewhere, filepath.Join(e.vol, ArchiveDir))
	res, err := e.o.Offload(context.Background(), e.req, nil)
	if err == nil || res.Verified {
		t.Fatal(res, err)
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Fatal("wrote through symlink")
	}
}
func TestOffloadParentSwapDoesNotRedirectWrites(t *testing.T) {
	e := setup(t, data(100))
	elsewhere := t.TempDir()
	archive := filepath.Join(e.vol, ArchiveDir)
	e.o.Copy = func(ctx context.Context, src, dst *os.File, p func(int64)) error {
		if err := os.Rename(archive, archive+".parked"); err != nil {
			return err
		}
		if err := os.Symlink(elsewhere, archive); err != nil {
			return err
		}
		return CopyFile(ctx, src, dst, p)
	}
	res, err := e.o.Offload(context.Background(), e.req, nil)
	if err == nil || res.Verified {
		t.Fatal("accepted redirected archive", res, err)
	}
	if !strings.Contains(res.Partial, ".parked/") {
		t.Fatal("actual partial path missing", res.Partial)
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Fatal("wrote into redirected directory")
	}
	retained(t, e, data(100))
}
func TestOffloadVerificationFailures(t *testing.T) {
	cases := map[string]CopyFunc{
		"corrupt": func(ctx context.Context, src, dst *os.File, p func(int64)) error {
			if err := CopyFile(ctx, src, dst, p); err != nil {
				return err
			}
			_, err := dst.WriteAt([]byte("!"), 0)
			return err
		},
		"short": func(ctx context.Context, src, dst *os.File, p func(int64)) error {
			if err := CopyFile(ctx, src, dst, p); err != nil {
				return err
			}
			return dst.Truncate(1)
		},
		"fails": func(context.Context, *os.File, *os.File, func(int64)) error { return errors.New("disk ejected") },
	}
	for name, cp := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t, data(100))
			e.o.Copy = cp
			res, err := e.o.Offload(context.Background(), e.req, nil)
			if err == nil || res.Verified || res.Partial == "" {
				t.Fatal(res, err)
			}
			pending, pendingErr := e.o.Journal.Pending()
			if pendingErr != nil || len(pending) != 1 || pending[0].RecoveryPath != res.Partial {
				t.Fatal("partial copy missing from recovery journal", pending, pendingErr)
			}
			retained(t, e, data(100))
			if err := RemoveSource(res); err == nil {
				t.Fatal("removed unverified original")
			}
		})
	}
}
func TestOffloadSourceChanged(t *testing.T) {
	e := setup(t, data(100))
	os.WriteFile(e.src, data(101), 0644)
	if _, err := e.o.Offload(context.Background(), e.req, nil); !errors.Is(err, fsid.ErrChanged) {
		t.Fatal(err)
	}
	e = setup(t, data(100))
	e.o.Copy = func(ctx context.Context, src, dst *os.File, p func(int64)) error {
		if err := CopyFile(ctx, src, dst, p); err != nil {
			return err
		}
		later := day.Add(time.Hour)
		return os.Chtimes(e.src, later, later)
	}
	if res, err := e.o.Offload(context.Background(), e.req, nil); err == nil || res.Verified {
		t.Fatal(res, err)
	}
}
func TestOffloadSpaceAndDevice(t *testing.T) {
	e := setup(t, data(100))
	e.o.Statfs = func(p string) (diskspace.Info, error) {
		return diskspace.Info{MountPoint: p, Free: 50 * units.MiB, Total: units.GiB}, nil
	}
	if _, err := e.o.Offload(context.Background(), e.req, nil); err == nil {
		t.Fatal("ignored headroom")
	}
	e = setup(t, data(100))
	e.o.AllowSameDevice = false
	if _, err := e.o.Offload(context.Background(), e.req, nil); err == nil {
		t.Fatal("allowed same device")
	}
}
func TestRemoveSourceRehashesCopy(t *testing.T) {
	e := setup(t, data(100))
	res := copied(t, e)
	b := bytes.Repeat([]byte("x"), int(res.Size))
	os.WriteFile(res.Dest, b, 0644)
	os.Chtimes(res.Dest, res.DestIdentity.ModTime(), res.DestIdentity.ModTime())
	// Even a refreshed stat snapshot cannot substitute for fresh verification.
	res.DestIdentity, _, _ = fsid.Of(res.Dest)
	if err := RemoveSource(res); err == nil {
		t.Fatal("removed original although copy data differs")
	}
	retained(t, e, data(100))
}
func TestCopyPreservesResourceForkAndAttributes(t *testing.T) {
	e := setup(t, data(100))
	if out, err := exec.Command("/usr/bin/xattr", "-w", "com.spacehound.test", "precious", e.src).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	fork := []byte("resource fork contents")
	if err := os.WriteFile(e.src+"/..namedfork/rsrc", fork, 0600); err != nil {
		t.Fatal(err)
	}
	e.refresh(t)
	res := copied(t, e)
	if b, err := os.ReadFile(res.Dest + "/..namedfork/rsrc"); err != nil || !bytes.Equal(b, fork) {
		t.Fatal("lost resource fork", err)
	}
	if out, err := exec.Command("/usr/bin/xattr", "-p", "com.spacehound.test", res.Dest).CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "precious" {
		t.Fatal("lost attribute", err, string(out))
	}
}
func TestMissingMetadataRefusesVerification(t *testing.T) {
	e := setup(t, data(100))
	if out, err := exec.Command("/usr/bin/xattr", "-w", "com.spacehound.test", "precious", e.src).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	e.refresh(t)
	e.o.Copy = func(ctx context.Context, src, dst *os.File, p func(int64)) error {
		_, err := io.Copy(dst, src)
		return err
	}
	res, err := e.o.Offload(context.Background(), e.req, nil)
	if err == nil || res.Verified {
		t.Fatal("verified incomplete metadata", res, err)
	}
	retained(t, e, data(100))
}
func TestSourceMetadataChangedAfterVerification(t *testing.T) {
	e := setup(t, data(100))
	res := copied(t, e)
	if out, err := exec.Command("/usr/bin/xattr", "-w", "com.spacehound.test", "new-only-on-source", e.src).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if err := RemoveSource(res); err == nil {
		t.Fatal("removed original with uncopied metadata")
	}
	retained(t, e, data(100))
}
func TestRemovalRefusesOpenWriterAndRestores(t *testing.T) {
	e := setup(t, data(100))
	res := copied(t, e)
	writer, err := os.OpenFile(e.src, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := RemoveSource(res); err == nil {
		t.Fatal("removed file held open by writer")
	}
	retained(t, e, data(100))
	pending, err := e.o.Journal.Pending()
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
}
func TestMissingJournalRefusesMutation(t *testing.T) {
	e := setup(t, data(100))
	e.o.Journal = nil
	if _, err := e.o.Offload(context.Background(), e.req, nil); err == nil {
		t.Fatal("copied without journal")
	}
	if entries, _ := os.ReadDir(e.vol); len(entries) != 0 {
		t.Fatal("destination changed")
	}
}
func TestCancelledRemovalKeepsOriginal(t *testing.T) {
	e := setup(t, data(100))
	res := copied(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RemoveSourceContext(ctx, res); err == nil {
		t.Fatal("ignored cancellation")
	}
	retained(t, e, data(100))
}
func TestDestinationVolumeReplaced(t *testing.T) {
	e := setup(t, data(100))
	res := copied(t, e)
	if err := os.Rename(e.vol, e.vol+"-old"); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(e.vol, 0755)
	if err := RemoveSource(res); err == nil {
		t.Fatal("accepted replacement volume")
	}
	retained(t, e, data(100))
}
