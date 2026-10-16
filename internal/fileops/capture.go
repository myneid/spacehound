// Package fileops holds an original safely while a confirmed operation is
// checked. A failed operation restores it without replacing another file.
package fileops

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"spacehound/internal/diskspace"
	"spacehound/internal/fsid"
	"spacehound/internal/macosfs"
	"spacehound/internal/oplog"
)

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// OpenRegular never resolves a symlink in the path, including its parents.
func OpenRegular(path string, want *fsid.Identity) (*os.File, error) {
	dir, err := macosfs.OpenDir(filepath.Dir(path), false, 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	f, err := macosfs.OpenAt(dir, filepath.Base(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", path)
	}
	if err == nil && want != nil {
		err = fsid.Check(fi, *want)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// CheckDir rejects a renamed/replaced parent before publishing an outcome.
func CheckDir(dir *os.File, path string) error {
	cur, err := macosfs.OpenDir(path, false, 0)
	if err != nil {
		return err
	}
	defer cur.Close()
	a, err := dir.Stat()
	if err != nil {
		return err
	}
	b, err := cur.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(a, b) {
		return fmt.Errorf("directory changed during operation: %s", path)
	}
	return nil
}

type Capture struct {
	Parent, Dir, File *os.File
	Source, DirName   string
	Entry             oplog.Entry
	Journal           *oplog.Logger
	active            bool
	originalIdentity  fsid.Identity
}

type RecoveryError struct {
	RecoveryPath string
	Err          error
}

func (e *RecoveryError) Error() string {
	return fmt.Sprintf("file preserved at %s: %v", e.RecoveryPath, e.Err)
}
func (e *RecoveryError) Unwrap() error { return e.Err }

// Preserve records an original that now lives outside the holding directory.
func (c *Capture) Preserve(path string, cause error) error {
	c.active = false
	return c.recordRecovery(path, cause)
}

func (c *Capture) recordRecovery(path string, cause error) error {
	if cause == nil {
		cause = errors.New("operation did not complete")
	}
	c.Entry.Phase, c.Entry.RecoveryPath, c.Entry.Error = "recovery", path, cause.Error()
	return &RecoveryError{path, errors.Join(cause, c.Journal.Log(c.Entry))}
}

func (c *Capture) RecoveryPath() string {
	dir, err := macosfs.Path(c.Dir)
	if err != nil {
		dir = filepath.Join(filepath.Dir(c.Source), c.DirName)
	}
	return filepath.Join(dir, "original")
}

// Take records recovery information before atomically capturing a source.
// A swapped source is preserved, never deleted, and restored when possible.
func Take(path string, want fsid.Identity, journal *oplog.Logger, action, dest string) (*Capture, error) {
	if err := journal.Check(); err != nil {
		return nil, err
	}
	parent, err := macosfs.OpenDir(filepath.Dir(path), false, 0)
	if err != nil {
		return nil, err
	}
	c := &Capture{Parent: parent, Source: path, DirName: ".spacehound-hold-" + ID(), Journal: journal}
	if err := macosfs.MkdirAt(parent, c.DirName, 0o700); err != nil {
		parent.Close()
		return nil, err
	}
	c.Dir, err = macosfs.OpenAt(parent, c.DirName, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		c.Close()
		return nil, err
	}
	if err := macosfs.MakePrivate(c.Dir, 0o700); err != nil {
		c.Close()
		return nil, err
	}
	fi, err := c.Dir.Stat()
	if err != nil || fi.Mode().Perm() != 0o700 || fi.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		c.Close()
		return nil, fmt.Errorf("cannot create a private source holding directory: %v", err)
	}
	c.Entry = oplog.Entry{ID: ID(), Action: action, Phase: "intent", Path: path, Dest: dest, Size: want.Size, RecoveryPath: c.RecoveryPath()}
	if info, err := diskspace.Statfs(filepath.Dir(path)); err == nil {
		c.Entry.FreeBefore = info.Free
	}
	if err := c.Dir.Sync(); err != nil {
		c.Close()
		return nil, err
	}
	if err := parent.Sync(); err != nil {
		c.Close()
		return nil, err
	}
	if err := journal.Log(c.Entry); err != nil {
		c.Close()
		return nil, err
	}
	f, err := OpenRegular(path, &want)
	if err == nil {
		err = CheckDir(parent, filepath.Dir(path))
		f.Close()
	}
	if err == nil {
		err = macosfs.Rename(parent, filepath.Base(path), c.Dir, "original")
	}
	if err != nil {
		_ = c.Complete(false, err)
		c.Close()
		return nil, err
	}
	c.active = true
	if err := c.Dir.Sync(); err != nil {
		err = c.Restore(err)
		c.Close()
		return nil, err
	}
	if err := parent.Sync(); err != nil {
		err = c.Restore(err)
		c.Close()
		return nil, err
	}
	c.File, err = macosfs.OpenAt(c.Dir, "original", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err == nil {
		var got fsid.Identity
		got, fi, err = fsid.FromOpen(c.File)
		c.originalIdentity = got
		// rename may change ctime; all other scan facts must still match.
		if err == nil && (!fi.Mode().IsRegular() || !got.SameFile(want) || got.Size != want.Size || got.MtimeNs != want.MtimeNs || got.Nlink != want.Nlink) {
			err = fmt.Errorf("%w: source was replaced while being captured", fsid.ErrChanged)
		}
	}
	if err == nil {
		c.Entry.Phase, c.Entry.RecoveryPath = "captured", c.RecoveryPath()
		err = journal.Log(c.Entry)
	}
	if err != nil {
		err = c.Restore(err)
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Capture) Complete(ok bool, cause error) error {
	e := c.Entry
	e.Phase, e.OK, e.RecoveryPath = "complete", ok, ""
	if info, err := diskspace.Statfs(filepath.Dir(c.Source)); err == nil {
		e.FreeAfter = info.Free
	}
	if cause != nil {
		e.Error = cause.Error()
	}
	return c.Journal.Log(e)
}

// Restore never overwrites a replacement at the source name.
func (c *Capture) Restore(cause error) error {
	if !c.active {
		return cause
	}
	if cause == nil {
		cause = errors.New("operation cancelled")
	}
	if err := CheckDir(c.Parent, filepath.Dir(c.Source)); err != nil {
		return c.recordRecovery(c.RecoveryPath(), errors.Join(cause, err))
	}
	entry, err := macosfs.OpenAt(c.Dir, "original", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err == nil {
		id, _, statErr := fsid.FromOpen(entry)
		entry.Close()
		err = statErr
		if err == nil && !id.SameFile(c.originalIdentity) {
			err = errors.New("holding entry was replaced; refusing to restore a different file")
		}
	}
	if err != nil {
		path := c.RecoveryPath()
		if c.File != nil {
			if actual, pathErr := macosfs.Path(c.File); pathErr == nil {
				path = actual
			}
		}
		return c.recordRecovery(path, errors.Join(cause, err))
	}
	if err := macosfs.Rename(c.Dir, "original", c.Parent, filepath.Base(c.Source)); err != nil {
		return c.recordRecovery(c.RecoveryPath(), errors.Join(cause, fmt.Errorf("restoration was blocked: %w", err)))
	}
	c.active = false
	if err := errors.Join(c.Parent.Sync(), c.Dir.Sync()); err != nil {
		return c.Preserve(c.Source, errors.Join(cause, err))
	}
	return errors.Join(cause, c.Complete(false, cause))
}

// Done marks a successful move or unlink; no recovery file remains.
func (c *Capture) Done() error {
	c.active = false
	if err := errors.Join(c.Dir.Sync(), c.Parent.Sync()); err != nil {
		return fmt.Errorf("operation completed; directory sync failed: %w", err)
	}
	return c.Complete(true, nil)
}

func (c *Capture) Close() {
	if c.File != nil {
		c.File.Close()
	}
	if c.Dir != nil {
		c.Dir.Close()
	}
	if c.Parent != nil {
		if !c.active && c.DirName != "" {
			_ = macosfs.RemoveDir(c.Parent, c.DirName)
		}
		c.Parent.Close()
	}
}
