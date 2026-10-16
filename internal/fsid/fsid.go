// Package fsid captures the on-disk identity of a file so that later
// destructive actions can refuse to operate on something that changed after
// it was scanned.
package fsid

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// Identity is the scan-time fingerprint of a regular file.
type Identity struct {
	Dev     uint64 `json:"dev"`
	Ino     uint64 `json:"ino"`
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	CtimeNs int64  `json:"ctime_ns"`
	Nlink   uint64 `json:"nlink"`
	// Allocated is the number of bytes actually allocated on disk
	// (st_blocks * 512). It may be smaller than Size for sparse files.
	Allocated int64 `json:"allocated"`
}

// ErrChanged is returned (wrapped) when a file no longer matches its
// recorded identity.
var ErrChanged = errors.New("file changed since scan")

// FromFileInfo extracts an Identity from an Lstat result.
func FromFileInfo(fi fs.FileInfo) (Identity, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, false
	}
	return Identity{
		Dev:       uint64(st.Dev),
		Ino:       uint64(st.Ino),
		Size:      fi.Size(),
		MtimeNs:   fi.ModTime().UnixNano(),
		CtimeNs:   st.Ctimespec.Sec*1e9 + st.Ctimespec.Nsec,
		Nlink:     uint64(st.Nlink),
		Allocated: int64(st.Blocks) * 512,
	}, true
}

// FromOpen returns the identity of the object held by f, without resolving
// its pathname again.
func FromOpen(f *os.File) (Identity, fs.FileInfo, error) {
	fi, err := f.Stat()
	if err != nil {
		return Identity{}, nil, err
	}
	id, ok := FromFileInfo(fi)
	if !ok {
		return Identity{}, fi, fmt.Errorf("%s: no stat information available", f.Name())
	}
	return id, fi, nil
}

// Of returns the identity of path without following symlinks.
func Of(path string) (Identity, fs.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Identity{}, nil, err
	}
	id, ok := FromFileInfo(fi)
	if !ok {
		return Identity{}, fi, fmt.Errorf("%s: no stat information available", path)
	}
	return id, fi, nil
}

// ModTime returns the recorded modification time.
func (id Identity) ModTime() time.Time { return time.Unix(0, id.MtimeNs) }

// SameFile reports whether two identities refer to the same inode.
func (id Identity) SameFile(o Identity) bool { return id.Dev == o.Dev && id.Ino == o.Ino }

// Revalidate re-stats path and returns an error wrapping ErrChanged unless
// it is still the same regular file, including change time and link count.
func Revalidate(path string, want Identity) error {
	_, fi, err := Of(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s no longer exists", ErrChanged, path)
		}
		return err
	}
	return Check(fi, want)
}

// Check compares an already obtained stat result with a recorded identity.
// Callers that hold an open descriptor can use this without following a path.
func Check(fi fs.FileInfo, want Identity) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is no longer a regular file (%s)", ErrChanged, fi.Name(), fi.Mode().Type())
	}
	cur, ok := FromFileInfo(fi)
	if !ok {
		return fmt.Errorf("%w: %s has no stat information", ErrChanged, fi.Name())
	}
	path := fi.Name()
	switch {
	case !cur.SameFile(want):
		return fmt.Errorf("%w: %s was replaced by a different file (inode/device differ)", ErrChanged, path)
	case cur.Size != want.Size:
		return fmt.Errorf("%w: %s size changed (%d -> %d bytes)", ErrChanged, path, want.Size, cur.Size)
	case cur.MtimeNs != want.MtimeNs:
		return fmt.Errorf("%w: %s was modified after the scan", ErrChanged, path)
	case cur.CtimeNs != want.CtimeNs:
		return fmt.Errorf("%w: %s contents or metadata changed after the scan", ErrChanged, path)
	case cur.Nlink != want.Nlink:
		return fmt.Errorf("%w: %s hard-link count changed", ErrChanged, path)
	}
	return nil
}
