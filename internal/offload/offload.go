// Package offload copies and verifies complete files before separately
// confirmed removal. All file access is anchored to directory descriptors.
package offload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"spacehound/internal/diskspace"
	"spacehound/internal/fileops"
	"spacehound/internal/fsid"
	"spacehound/internal/macosfs"
	"spacehound/internal/oplog"
	"spacehound/internal/scan"
	"spacehound/internal/volumes"
)

const ArchiveDir = "SpaceHound Archive"

var (
	ErrDestExists = errors.New("destination already exists")
	ErrVerify     = errors.New("copy verification failed")
)

// CopyFunc receives already-open files; it cannot redirect their paths.
type CopyFunc func(context.Context, *os.File, *os.File, func(int64)) error
type ProgressFunc func(phase string, done, total int64)

type Offloader struct {
	Home, Host      string
	Now             func() time.Time
	Copy            CopyFunc
	Journal         *oplog.Logger
	Statfs          func(string) (diskspace.Info, error)
	AllowSameDevice bool // Generated fixtures only.
}

func New(home string) *Offloader {
	return &Offloader{Home: home, Host: Hostname(), Copy: CopyFile, Statfs: diskspace.Statfs}
}

type Request struct {
	Source   string
	Identity fsid.Identity
	Volume   volumes.Volume
}

type Result struct {
	Source         string              `json:"source"`
	Dest           string              `json:"dest"`
	Partial        string              `json:"partial,omitempty"`
	RecoveryPath   string              `json:"recovery_path,omitempty"`
	Size           int64               `json:"size"`
	SHA256         string              `json:"sha256,omitempty"`
	Verified       bool                `json:"verified"`
	SourceIdentity fsid.Identity       `json:"-"`
	DestIdentity   fsid.Identity       `json:"-"`
	Fingerprint    macosfs.Fingerprint `json:"-"`
	VolumePath     string              `json:"-"`
	VolumeIdentity fsid.Identity       `json:"-"`
	Journal        *oplog.Logger       `json:"-"`
}

// Hostname returns a short, filesystem-safe host name.
func Hostname() string {
	h, _ := os.Hostname()
	h = strings.TrimSuffix(h, ".local")
	return SanitizeName(h)
}

// SanitizeName makes s usable as a single path component.
func SanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '/' || r == ':' || r == '\\' || r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" || strings.Trim(s, ".") == "" {
		return "Mac"
	}
	return s
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

// DestPath returns <vol>/SpaceHound Archive/<host>/<YYYY-MM-DD>/<rel>, where
// rel is the source path relative to home (or "_outside_home/<abs path>").
// It rejects anything that could escape the archive folder.
func DestPath(vol, host string, date time.Time, home, src string) (string, error) {
	if !filepath.IsAbs(src) || filepath.Clean(src) != src {
		return "", fmt.Errorf("source must be a clean absolute path: %q", src)
	}
	if !filepath.IsAbs(vol) || filepath.Clean(vol) != vol || vol == "/" {
		return "", fmt.Errorf("invalid volume path %q", vol)
	}
	var rel string
	if home != "" && src != home && strings.HasPrefix(src, filepath.Clean(home)+"/") {
		rel = src[len(filepath.Clean(home))+1:]
	} else {
		rel = filepath.Join("_outside_home", strings.TrimPrefix(src, "/"))
	}
	for _, c := range strings.Split(rel, "/") {
		if c == "" || c == "." || c == ".." {
			return "", fmt.Errorf("unsafe path component %q in %q", c, src)
		}
	}
	base := filepath.Join(vol, ArchiveDir, SanitizeName(host), date.Format("2006-01-02"))
	dest := filepath.Join(base, rel)
	if !within(dest, base) || dest == base {
		return "", fmt.Errorf("destination %q escapes %q", dest, base)
	}
	return dest, nil
}

func (o *Offloader) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}
func (o *Offloader) Plan(req Request) (string, error) {
	return DestPath(req.Volume.Path, o.Host, o.now(), o.Home, req.Source)
}

func (o *Offloader) Offload(ctx context.Context, req Request, progress ProgressFunc) (res Result, retErr error) {
	res = Result{Source: req.Source, Size: req.Identity.Size, SourceIdentity: req.Identity, Journal: o.Journal, VolumePath: req.Volume.Path}
	if progress == nil {
		progress = func(string, int64, int64) {}
	}
	if why := scan.ProtectedReason(req.Source, o.Home); why != "" {
		return res, fmt.Errorf("protected source: %s", why)
	}
	if err := o.Journal.Check(); err != nil {
		return res, err
	}
	source, err := fileops.OpenRegular(req.Source, &req.Identity)
	if err != nil {
		return res, err
	}
	defer source.Close()
	if req.Identity.Nlink != 1 {
		return res, errors.New("offloading requires a file with exactly one hard link")
	}
	vol := req.Volume
	root, err := macosfs.OpenDir(vol.Path, false, 0)
	if err != nil {
		return res, err
	}
	defer root.Close()
	vid, _, err := fsid.FromOpen(root)
	if err != nil {
		return res, err
	}
	res.VolumeIdentity = vid
	if !o.AllowSameDevice && (vid.Dev != vol.Dev || vid.Dev == req.Identity.Dev) {
		return res, errors.New("destination volume changed or is on the source filesystem")
	}
	if o.Statfs != nil {
		info, err := o.Statfs(vol.Path)
		if err != nil {
			return res, fmt.Errorf("destination unavailable: %w", err)
		}
		if info.ReadOnly() || (!o.AllowSameDevice && info.MountPoint != vol.Path) {
			return res, errors.New("destination is read-only or no longer mounted")
		}
		vol.Free, vol.Total = info.Free, info.Total
	}
	if err := vol.CanHold(req.Identity.Size); err != nil {
		return res, err
	}
	dest, err := o.Plan(req)
	if err != nil {
		return res, err
	}
	res.Dest = dest
	dirPath := filepath.Dir(dest)
	dir, err := macosfs.OpenDir(dirPath, true, 0o755)
	if err != nil {
		return res, err
	}
	defer dir.Close()
	// Opening from / is no-follow; also reject a changed volume root.
	if err := fileops.CheckDir(root, vol.Path); err != nil {
		return res, err
	}
	if exists, err := macosfs.ExistsAt(dir, filepath.Base(dest)); err != nil {
		return res, err
	} else if exists {
		return res, ErrDestExists
	}
	// Capture a stable, complete source fingerprint before making any copy.
	before, err := macosfs.FingerprintFile(ctx, source, nil)
	if err != nil {
		return res, err
	}
	if fi, err := source.Stat(); err != nil {
		return res, err
	} else if err := fsid.Check(fi, req.Identity); err != nil {
		return res, err
	}
	partial := ".spacehound-partial-" + fileops.ID()
	entry := oplog.Entry{ID: fileops.ID(), Action: "offload-copy", Phase: "intent", Path: req.Source, Dest: dest, Size: req.Identity.Size, RecoveryPath: filepath.Join(dirPath, partial)}
	if info, err := diskspace.Statfs(filepath.Dir(req.Source)); err == nil {
		entry.FreeBefore = info.Free
	}
	if err := o.Journal.Log(entry); err != nil {
		return res, err
	}
	defer func() {
		entry.Phase, entry.OK = "complete", retErr == nil
		if info, err := diskspace.Statfs(filepath.Dir(req.Source)); err == nil {
			entry.FreeAfter = info.Free
		}
		entry.RecoveryPath = ""
		if retErr != nil {
			entry.Error = retErr.Error()
			if res.Partial != "" {
				entry.Phase, entry.RecoveryPath = "recovery", res.Partial
			}
		}
		if err := o.Journal.Log(entry); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	out, err := macosfs.OpenAt(dir, partial, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return res, err
	}
	defer out.Close()
	partialPath := func() string {
		p, err := macosfs.Path(dir)
		if err != nil {
			p = dirPath
		}
		return filepath.Join(p, partial)
	}
	failed := func(err error) (Result, error) {
		res.Partial = partialPath()
		return res, fmt.Errorf("%w; original retained; copy preserved at %s", err, res.Partial)
	}
	if o.Copy == nil {
		return failed(errors.New("no metadata-preserving copy method configured"))
	}
	progress("copying", 0, req.Identity.Size)
	if err := o.Copy(ctx, source, out, func(n int64) { progress("copying", n, req.Identity.Size) }); err != nil {
		return failed(err)
	}
	after, err := macosfs.FingerprintFile(ctx, source, func(n int64) { progress("verifying", n, 2*req.Identity.Size) })
	if err != nil {
		return failed(err)
	}
	copied, err := macosfs.FingerprintFile(ctx, out, func(n int64) { progress("verifying", req.Identity.Size+n, 2*req.Identity.Size) })
	if err != nil {
		return failed(err)
	}
	if before != after {
		return failed(fmt.Errorf("%w: source data or metadata changed during copying", ErrVerify))
	}
	if after != copied {
		return failed(fmt.Errorf("%w: destination data or metadata differs (source %s/%s, copy %s/%s)", ErrVerify, after.SHA256, after.MetadataSHA256, copied.SHA256, copied.MetadataSHA256))
	}
	entry.SHA256 = copied.SHA256
	if fi, err := source.Stat(); err != nil {
		return failed(err)
	} else if err := fsid.Check(fi, req.Identity); err != nil {
		return failed(err)
	}
	if err := fileops.CheckDir(dir, dirPath); err != nil {
		return failed(err)
	}
	if err := macosfs.FullSync(out); err != nil {
		return failed(fmt.Errorf("cannot flush destination: %w", err))
	}
	if err := macosfs.Rename(dir, partial, dir, filepath.Base(dest)); err != nil {
		if errors.Is(err, os.ErrExist) {
			err = fmt.Errorf("%w: %s", ErrDestExists, dest)
		}
		return failed(err)
	}
	partial = filepath.Base(dest)
	if err := dir.Sync(); err != nil {
		return failed(err)
	}
	if err := macosfs.FullSync(out); err != nil {
		return failed(fmt.Errorf("cannot flush published destination: %w", err))
	}
	if err := fileops.CheckDir(root, vol.Path); err != nil {
		return failed(err)
	}
	if err := fileops.CheckDir(dir, dirPath); err != nil {
		return failed(err)
	}
	did, _, err := fsid.FromOpen(out)
	if err != nil {
		return failed(err)
	}
	check, err := fileops.OpenRegular(dest, &did)
	if err != nil {
		return failed(err)
	}
	check.Close()
	res.DestIdentity, res.Fingerprint, res.SHA256, res.Verified = did, copied, copied.SHA256, true
	return res, nil
}

// CopyFile copies data with cancellation, then all macOS file metadata.
func CopyFile(ctx context.Context, src, dst *os.File, progress func(int64)) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(dst, &copyReader{ctx: ctx, r: src, step: progress}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return macosfs.CopyMetadata(src, dst)
}

type copyReader struct {
	ctx  context.Context
	r    io.Reader
	step func(int64)
	n    int64
}

func (r *copyReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(b)
	r.n += int64(n)
	if r.step != nil {
		r.step(r.n)
	}
	return n, err
}

func checkCopy(ctx context.Context, path string, want fsid.Identity, fingerprint macosfs.Fingerprint) (*os.File, error) {
	f, err := fileops.OpenRegular(path, &want)
	if err != nil {
		return nil, err
	}
	got, err := macosfs.FingerprintFile(ctx, f, nil)
	if err == nil {
		fi, statErr := f.Stat()
		err = statErr
		if err == nil {
			err = fsid.Check(fi, want)
		}
	}
	if err == nil && got != fingerprint {
		err = fmt.Errorf("%w: data or metadata changed: %s", ErrVerify, path)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func CheckRemovable(res Result) error { return CheckRemovableContext(context.Background(), res) }
func CheckRemovableContext(ctx context.Context, res Result) error {
	if !res.Verified || res.Fingerprint.SHA256 == "" {
		return errors.New("no complete verified copy; original must be retained")
	}
	root, err := macosfs.OpenDir(res.VolumePath, false, 0)
	if err != nil {
		return err
	}
	id, _, err := fsid.FromOpen(root)
	root.Close()
	if err != nil {
		return err
	}
	if !id.SameFile(res.VolumeIdentity) {
		return errors.New("destination volume changed; original retained")
	}
	for _, entry := range []struct {
		path string
		id   fsid.Identity
	}{{res.Source, res.SourceIdentity}, {res.Dest, res.DestIdentity}} {
		f, err := checkCopy(ctx, entry.path, entry.id, res.Fingerprint)
		if err != nil {
			return err
		}
		f.Close()
	}
	return nil
}
func RemoveSource(res Result) error { return RemoveSourceContext(context.Background(), res) }
func RemoveSourceContext(ctx context.Context, res Result) error {
	if err := res.Journal.Check(); err != nil {
		return err
	}
	if err := CheckRemovableContext(ctx, res); err != nil {
		return err
	}
	parent, err := macosfs.OpenDir(filepath.Dir(res.Source), false, 0)
	if err != nil {
		return err
	}
	err = macosfs.CheckGuardedUnlink(parent)
	parent.Close()
	if err != nil {
		return fmt.Errorf("permanent removal unavailable; use Trash or keep both: %w", err)
	}
	capture, err := fileops.Take(res.Source, res.SourceIdentity, res.Journal, "offload-remove-source", res.Dest)
	if err != nil {
		return err
	}
	defer capture.Close()
	fail := func(err error) error { return capture.Restore(err) }
	capture.Entry.SHA256 = res.SHA256
	originalID, _, err := fsid.FromOpen(capture.File)
	if err != nil {
		return fail(err)
	}
	current, err := macosfs.FingerprintFile(ctx, capture.File, nil)
	if err != nil {
		return fail(err)
	}
	if current != res.Fingerprint {
		return fail(fmt.Errorf("%w: captured original changed", ErrVerify))
	}
	dest, err := checkCopy(ctx, res.Dest, res.DestIdentity, res.Fingerprint)
	if err != nil {
		return fail(err)
	}
	defer dest.Close()
	err = macosfs.FullSync(dest)
	if err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if fi, err := capture.File.Stat(); err != nil {
		return fail(err)
	} else if err := fsid.Check(fi, originalID); err != nil {
		return fail(err)
	}
	if fi, err := dest.Stat(); err != nil {
		return fail(err)
	} else if err := fsid.Check(fi, res.DestIdentity); err != nil {
		return fail(err)
	}
	if err := fileops.CheckDir(capture.Parent, filepath.Dir(res.Source)); err != nil {
		return fail(err)
	}
	if err := fileops.CheckDir(capture.Dir, filepath.Dir(capture.RecoveryPath())); err != nil {
		return fail(err)
	}
	if err := capture.File.Close(); err != nil {
		return fail(err)
	}
	capture.File = nil
	if err := macosfs.Unlink(capture.Dir, "original", true); err != nil {
		return fail(err)
	}
	if err := capture.Done(); err != nil {
		return fmt.Errorf("original removed; journal completion failed: %w", err)
	}
	return nil
}
