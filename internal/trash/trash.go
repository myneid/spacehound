// Package trash moves regular files to same-volume Trash with exclusive,
// descriptor-relative renames. It never permanently unlinks an original.
package trash

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"spacehound/internal/fileops"
	"spacehound/internal/fsid"
	"spacehound/internal/macosfs"
	"spacehound/internal/oplog"
	"spacehound/internal/scan"
)

var ErrNoTrash = errors.New("no usable Trash for this file")

type Trasher struct {
	Home    string
	Journal *oplog.Logger
	DirFor  func(string, uint64) (string, error)
	Now     func() time.Time
	// move allows deterministic filesystem-failure tests; nil uses the native
	// atomic exclusive operation. Production never substitutes ordinary rename.
	move func(*os.File, string, *os.File, string) error
}

func New(home string) *Trasher { return &Trasher{Home: home} }

type Result struct {
	Path         string `json:"path"`
	Dest         string `json:"dest,omitempty"`
	RecoveryPath string `json:"recovery_path,omitempty"`
	Method       string `json:"method"`
}

func (t *Trasher) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}
func (t *Trasher) dirFor(path string, dev uint64) (string, error) {
	if t.DirFor != nil {
		return t.DirFor(path, dev)
	}
	home := filepath.Join(t.Home, ".Trash")
	if dir, err := macosfs.OpenDir(home, false, 0); err == nil {
		id, _, err := fsid.FromOpen(dir)
		dir.Close()
		if err == nil && id.Dev == dev {
			return home, nil
		}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoTrash, err)
	}
	mnt := cString(st.Mntonname[:])
	if mnt == "" || mnt == "/" {
		return "", ErrNoTrash
	}
	root, err := macosfs.OpenDir(filepath.Join(mnt, ".Trashes"), false, 0)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoTrash, err)
	}
	defer root.Close()
	name := fmt.Sprint(os.Getuid())
	if err := macosfs.MkdirAt(root, name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	return filepath.Join(mnt, ".Trashes", name), nil
}
func cString(b []int8) string {
	var out []byte
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// CandidateName returns the n-th collision-safe name for base: n=0 is the
// original name, then "<stem> (SpaceHound <stamp>)<ext>", then the same with
// " 2", " 3"...
func CandidateName(base string, stamp time.Time, n int) string {
	if n == 0 {
		return base
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" { // dotfile such as ".env"
		stem, ext = base, ""
	}
	tag := "SpaceHound " + stamp.Format("2006-01-02 15.04.05")
	if n > 1 {
		tag += fmt.Sprintf(" %d", n)
	}
	return fmt.Sprintf("%s (%s)%s", stem, tag, ext)
}

func (t *Trasher) Trash(path string, want fsid.Identity) (Result, error) {
	res := Result{Path: path}
	move := t.move
	if move == nil {
		move = macosfs.Rename
	}
	if !filepath.IsAbs(path) {
		return res, fmt.Errorf("refusing relative path %q", path)
	}
	if why := scan.ProtectedReason(path, t.Home); why != "" {
		return res, fmt.Errorf("protected source: %s", why)
	}
	if err := t.Journal.Check(); err != nil {
		return res, err
	}
	source, err := fileops.OpenRegular(path, &want)
	if err != nil {
		return res, err
	}
	source.Close()
	dirPath, err := t.dirFor(path, want.Dev)
	if err != nil {
		return res, err
	}
	dir, err := macosfs.OpenDir(dirPath, false, 0)
	if err != nil {
		return res, fmt.Errorf("%w: %v", ErrNoTrash, err)
	}
	defer dir.Close()
	if err := macosfs.MakePrivate(dir, 0o700); err != nil {
		return res, fmt.Errorf("%w: unsafe per-user Trash: %v", ErrNoTrash, err)
	}
	did, fi, err := fsid.FromOpen(dir)
	if err != nil {
		return res, err
	}
	if !fi.IsDir() || did.Dev != want.Dev {
		return res, fmt.Errorf("%w: Trash is not on the source filesystem", ErrNoTrash)
	}
	capture, err := fileops.Take(path, want, t.Journal, "trash", dirPath)
	if err != nil {
		return res, err
	}
	defer capture.Close()
	stamp := t.now()
	capturedID, _, err := fsid.FromOpen(capture.File)
	if err != nil {
		return res, capture.Restore(err)
	}
	for n := 0; n < 1000; n++ {
		name := CandidateName(filepath.Base(path), stamp, n)
		if err := fileops.CheckDir(dir, dirPath); err != nil {
			return res, capture.Restore(err)
		}
		capture.Entry.Dest = filepath.Join(dirPath, name)
		if err := t.Journal.Log(capture.Entry); err != nil {
			return res, capture.Restore(err)
		}
		if err := move(capture.Dir, "original", dir, name); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return res, capture.Restore(err)
		}
		actual, _ := macosfs.Path(dir)
		if actual == "" {
			actual = dirPath
		}
		res.Dest = filepath.Join(actual, name)
		moved, err := macosfs.OpenAt(dir, name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			id, fi, statErr := fsid.FromOpen(moved)
			moved.Close()
			err = statErr
			if err == nil && (!fi.Mode().IsRegular() || !id.SameFile(capturedID) || id.Size != capturedID.Size || id.MtimeNs != capturedID.MtimeNs || id.Nlink != capturedID.Nlink) {
				err = fmt.Errorf("%w: trashed item changed", fsid.ErrChanged)
			}
		}
		if err == nil {
			err = fileops.CheckDir(dir, dirPath)
		}
		if err == nil {
			err = dir.Sync()
		}
		if err == nil {
			err = capture.Dir.Sync()
		}
		if err != nil {
			// A shared Trash pathname can be replaced even between an
			// identity check and rollback. Preserve the descriptor-identified
			// original instead of ever renaming that pathname back.
			actual, pathErr := macosfs.Path(capture.File)
			if pathErr != nil {
				actual = res.Dest
			}
			res.RecoveryPath = actual
			return res, capture.Preserve(actual, errors.Join(err, pathErr))
		}

		res.Method = "moved to " + res.Dest + "; restore manually from this location"
		if err := capture.Done(); err != nil {
			return res, fmt.Errorf("moved to %s; journal completion failed: %w", res.Dest, err)
		}
		return res, nil
	}
	return res, capture.Restore(errors.New("no free Trash filename"))
}
