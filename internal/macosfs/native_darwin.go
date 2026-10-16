//go:build darwin && cgo

// Package macosfs provides descriptor-relative filesystem operations. It never
// substitutes a check followed by an overwriting rename for an exclusive move.
package macosfs

/*
#include <sys/types.h>
#include <sys/stat.h>
#include <sys/attr.h>
#include <sys/acl.h>
#include <sys/xattr.h>
#include <fcntl.h>
#include <copyfile.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <errno.h>
#include <limits.h>

// These public flag values also allow building with an older SDK. Whether the
// running kernel and filesystem enforce them is checked with disposable files.
#ifndef AT_NODELETEBUSY
#define AT_NODELETEBUSY 0x4000
#endif
#ifndef AT_UNIQUE
#define AT_UNIQUE 0x8000
#endif
#ifndef ENOTCAPABLE
#define ENOTCAPABLE 107
#endif

static int sh_openat(int dir, const char *name, int flags, mode_t mode, int *fd) {
    *fd = openat(dir, name, flags | O_NOFOLLOW | O_CLOEXEC, mode);
    return *fd < 0 ? errno : 0;
}
static int sh_mkdirat(int dir, const char *name, mode_t mode) {
    return mkdirat(dir, name, mode) < 0 ? errno : 0;
}
static int sh_existsat(int dir, const char *name, int *exists) {
    struct stat st;
    if (fstatat(dir, name, &st, AT_SYMLINK_NOFOLLOW) == 0) {
        *exists = 1;
        return 0;
    }
    if (errno != ENOENT) return errno;
    *exists = 0;
    return 0;
}
static int sh_rename(int fromdir, const char *from, int todir, const char *to) {
    return renameatx_np(fromdir, from, todir, to, RENAME_EXCL) < 0 ? errno : 0;
}
static int sh_unlink(int dir, const char *name, int guarded) {
    int flags = guarded ? (AT_NODELETEBUSY | AT_UNIQUE) : 0;
    return unlinkat(dir, name, flags) < 0 ? errno : 0;
}
static int sh_rmdir(int dir, const char *name) {
    return unlinkat(dir, name, AT_REMOVEDIR) < 0 ? errno : 0;
}
static int sh_link(int dir, const char *from, const char *to) {
    return linkat(dir, from, dir, to, 0) < 0 ? errno : 0;
}
static int sh_metadata(int src, int dst) {
    struct stat source;
    if (fstat(src, &source) < 0) return errno;
    copyfile_state_t state = copyfile_state_alloc();
    if (state == NULL) return errno ? errno : ENOMEM;
    uint32_t preserve_suid = 1;
    int result = copyfile_state_set(state, COPYFILE_STATE_PRESERVE_SUID, &preserve_suid);
    if (result == 0) result = fcopyfile(src, dst, state, COPYFILE_METADATA);
    int saved = result < 0 ? errno : 0;
    copyfile_state_free(state);
    if (saved) return saved;
    // COPYFILE_STAT can clamp the new file's birthtime to its copied mtime,
    // rather than preserve a distinct, older source creation time.
    struct attrlist attrs = {0};
    attrs.bitmapcount = ATTR_BIT_MAP_COUNT;
    attrs.commonattr = ATTR_CMN_CRTIME;
    struct timespec birth = source.st_birthtimespec;
    return fsetattrlist(dst, &attrs, &birth, sizeof(birth), 0) < 0 ? errno : 0;
}
static int sh_fullsync(int fd) {
    return fcntl(fd, F_FULLFSYNC) < 0 ? errno : 0;
}
static int sh_getpath(int fd, char *path) {
    return fcntl(fd, F_GETPATH, path) < 0 ? errno : 0;
}
static int sh_xattr_names(int fd, char *buf, size_t size, ssize_t *n) {
    *n = flistxattr(fd, buf, size, 0);
    return *n < 0 ? errno : 0;
}
static int sh_xattr(int fd, const char *name, void *buf, size_t size, uint32_t offset, ssize_t *n) {
    *n = fgetxattr(fd, name, buf, size, offset, 0);
    return *n < 0 ? errno : 0;
}
static int sh_acl(int fd, void **buf, ssize_t *size) {
    acl_t acl = acl_get_fd_np(fd, ACL_TYPE_EXTENDED);
    // Darwin reports ENOENT when the descriptor exists but has no extended
    // ACL. Normalize that to the same canonical empty ACL as acl_init(0).
    if (acl == NULL && errno == ENOENT) acl = acl_init(0);
    if (acl == NULL) return errno;
    *size = acl_size(acl);
    if (*size < 0) { int saved = errno; acl_free(acl); return saved; }
    // Zero padding as well as the defined fields before serializing.
    *buf = calloc(1, *size);
    if (*buf == NULL) { acl_free(acl); return ENOMEM; }
    *size = acl_copy_ext(*buf, acl, *size);
    int saved = *size < 0 ? errno : 0;
    acl_free(acl);
    if (saved) { free(*buf); *buf = NULL; }
    return saved;
}
static int sh_private(int fd, mode_t mode) {
    acl_t acl = acl_init(0);
    if (acl == NULL) return errno;
    int result = acl_set_fd_np(fd, acl, ACL_TYPE_EXTENDED);
    int saved = result < 0 ? errno : 0;
    acl_free(acl);
    if (saved) return saved;
    return fchmod(fd, mode) < 0 ? errno : 0;
}
static int sh_acl_empty(int fd, int *empty) {
    acl_t acl = acl_get_fd_np(fd, ACL_TYPE_EXTENDED);
    if (acl == NULL && errno == ENOENT) { *empty = 1; return 0; }
    if (acl == NULL) return errno;
    acl_entry_t entry;
    int result = acl_get_entry(acl, ACL_FIRST_ENTRY, &entry);
    // Darwin returns zero for an entry, and -1/EINVAL at the end of a valid
    // ACL (unlike implementations that return 1/0 for entry/end).
    *empty = result < 0 && errno == EINVAL;
    int saved = result < 0 && !*empty ? errno : 0;
    acl_free(acl);
    return saved;
}
static unsigned int sh_preservable_flags(void) {
    return UF_NODUMP | UF_IMMUTABLE | UF_APPEND | UF_OPAQUE | UF_HIDDEN |
        SF_ARCHIVED | SF_IMMUTABLE | SF_APPEND | SF_NOUNLINK;
}
*/
import "C"

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

var (
	// ErrChanged means a file changed while its fingerprint was being read.
	ErrChanged = errors.New("file changed during verification")
	// ErrGuardedUnlinkUnsupported means a probe could not prove that both busy
	// files (including writable mappings) and multiply linked files are protected.
	ErrGuardedUnlinkUnsupported = errors.New("filesystem does not enforce guarded removal")
)

func component(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("unsafe path component %q", name)
	}
	return nil
}

func errnoError(op, path string, code C.int) error {
	if code == 0 {
		return nil
	}
	return &os.PathError{Op: op, Path: path, Err: syscall.Errno(code)}
}

// OpenDir opens an absolute, clean directory path without following any
// symlink. When create is true missing components are created with mode.
func OpenDir(path string, create bool, mode os.FileMode) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return nil, fmt.Errorf("directory must be a clean absolute path: %q", path)
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), "/")
	if path == "/" {
		return dir, nil
	}
	for _, name := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, openErr := OpenAt(dir, name, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
		if create && errors.Is(openErr, os.ErrNotExist) {
			mkdirErr := MkdirAt(dir, name, mode)
			if mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				dir.Close()
				return nil, mkdirErr
			}
			// Commit each created ancestor, not only the deepest archive
			// directory. Otherwise a durable file could lose its pathname.
			if mkdirErr == nil {
				if err := SyncDir(dir); err != nil {
					dir.Close()
					return nil, err
				}
			}
			next, openErr = OpenAt(dir, name, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
		}
		dir.Close()
		if openErr != nil {
			return nil, openErr
		}
		dir = next
	}
	return dir, nil
}

// OpenAt opens one child of dir with O_NOFOLLOW and O_CLOEXEC. Callers must
// check its type before reading or modifying it. O_NONBLOCK avoids FIFO waits.
func OpenAt(dir *os.File, name string, flags int, mode os.FileMode) (*os.File, error) {
	defer runtime.KeepAlive(dir)
	if err := component(name); err != nil {
		return nil, err
	}
	n := C.CString(name)
	defer C.free(unsafe.Pointer(n))
	var fd C.int
	if err := errnoError("openat", name, C.sh_openat(C.int(dir.Fd()), n, C.int(flags), C.mode_t(mode.Perm()), &fd)); err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name)), nil
}

// ExistsAt checks one entry without following symlinks or opening its contents.
// Callers still need an exclusive operation to reject later replacements.
func ExistsAt(dir *os.File, name string) (bool, error) {
	defer runtime.KeepAlive(dir)
	if err := component(name); err != nil {
		return false, err
	}
	n := C.CString(name)
	defer C.free(unsafe.Pointer(n))
	var exists C.int
	if err := errnoError("fstatat", name, C.sh_existsat(C.int(dir.Fd()), n, &exists)); err != nil {
		return false, err
	}
	return exists != 0, nil
}

// MkdirAt creates one directory entry and never follows an existing entry.
func MkdirAt(dir *os.File, name string, mode os.FileMode) error {
	defer runtime.KeepAlive(dir)
	if err := component(name); err != nil {
		return err
	}
	n := C.CString(name)
	defer C.free(unsafe.Pointer(n))
	return errnoError("mkdirat", name, C.sh_mkdirat(C.int(dir.Fd()), n, C.mode_t(mode.Perm())))
}

// Rename atomically moves one entry and refuses to overwrite any destination.
// Filesystems without exclusive-rename support return an error.
func Rename(fromDir *os.File, from string, toDir *os.File, to string) error {
	defer runtime.KeepAlive(fromDir)
	defer runtime.KeepAlive(toDir)
	if err := component(from); err != nil {
		return err
	}
	if err := component(to); err != nil {
		return err
	}
	f, t := C.CString(from), C.CString(to)
	defer C.free(unsafe.Pointer(f))
	defer C.free(unsafe.Pointer(t))
	return errnoError("exclusive rename", from+" -> "+to, C.sh_rename(C.int(fromDir.Fd()), f, C.int(toDir.Fd()), t))
}

// Unlink removes a single entry. guarded requires no open descriptors or
// mappings and exactly one hard link; callers must first probe the filesystem
// with CheckGuardedUnlink and close their own file descriptor before deleting.
func Unlink(dir *os.File, name string, guarded bool) error {
	defer runtime.KeepAlive(dir)
	if err := component(name); err != nil {
		return err
	}
	n := C.CString(name)
	defer C.free(unsafe.Pointer(n))
	g := C.int(0)
	if guarded {
		g = 1
	}
	return errnoError("unlinkat", name, C.sh_unlink(C.int(dir.Fd()), n, g))
}

// RemoveDir removes one empty child directory.
func RemoveDir(dir *os.File, name string) error {
	defer runtime.KeepAlive(dir)
	if err := component(name); err != nil {
		return err
	}
	n := C.CString(name)
	defer C.free(unsafe.Pointer(n))
	return errnoError("rmdirat", name, C.sh_rmdir(C.int(dir.Fd()), n))
}

// CopyMetadata copies native metadata, including extended attributes, resource
// forks, ACLs and stat fields, between already opened files. Verify the result
// with FingerprintFile: native copying may omit metadata a filesystem rejects.
func CopyMetadata(src, dst *os.File) error {
	defer runtime.KeepAlive(src)
	defer runtime.KeepAlive(dst)
	return errnoError("copy metadata", dst.Name(), C.sh_metadata(C.int(src.Fd()), C.int(dst.Fd())))
}

// MakePrivate removes inherited access grants as well as restricting POSIX
// permissions. Only a caller-owned directory (0700) or single-link regular
// file (0600) may be changed; no pathname is resolved during this operation.
func MakePrivate(f *os.File, mode os.FileMode) error {
	defer runtime.KeepAlive(f)
	before, err := stat(f)
	if err != nil {
		return err
	}
	kind := before.Mode & syscall.S_IFMT
	if before.Uid != uint32(os.Getuid()) ||
		!((kind == syscall.S_IFDIR && mode == 0o700) ||
			(kind == syscall.S_IFREG && mode == 0o600 && before.Nlink == 1)) {
		return fmt.Errorf("unsafe ownership, type, link count or private mode for %s", f.Name())
	}
	var empty C.int
	if err := errnoError("check private ACL", f.Name(), C.sh_acl_empty(C.int(f.Fd()), &empty)); err != nil {
		return err
	}
	changed := before.Mode&0o7777 != uint16(mode) || empty == 0
	if changed {
		if err := errnoError("make private", f.Name(), C.sh_private(C.int(f.Fd()), C.mode_t(mode))); err != nil {
			return err
		}
	}
	if err := errnoError("verify private ACL", f.Name(), C.sh_acl_empty(C.int(f.Fd()), &empty)); err != nil {
		return err
	}
	after, err := stat(f)
	if err != nil {
		return err
	}
	if after.Dev != before.Dev || after.Ino != before.Ino || after.Uid != before.Uid ||
		after.Mode&0o7777 != uint16(mode) || after.Mode&syscall.S_IFMT != kind ||
		(kind == syscall.S_IFREG && after.Nlink != 1) || empty == 0 {
		return fmt.Errorf("could not verify private permissions for %s", f.Name())
	}
	if !changed && kind == syscall.S_IFREG && !stable(before, after) {
		return fmt.Errorf("%w: private file %s", ErrChanged, f.Name())
	}
	return nil
}

// FullSync asks the device to make a file's contents and metadata durable.
// Failure is returned rather than substituting a weaker synchronization.
func FullSync(f *os.File) error {
	defer runtime.KeepAlive(f)
	return errnoError("full sync", f.Name(), C.sh_fullsync(C.int(f.Fd())))
}

// SyncDir commits directory-entry changes to the filesystem.
func SyncDir(dir *os.File) error { return dir.Sync() }

// Path obtains the current kernel path of an open descriptor. File.Name may
// be stale after a directory rename; recovery records should use this path.
func Path(f *os.File) (string, error) {
	defer runtime.KeepAlive(f)
	var path [C.PATH_MAX]C.char
	if err := errnoError("get descriptor path", f.Name(), C.sh_getpath(C.int(f.Fd()), &path[0])); err != nil {
		return "", err
	}
	return C.GoString(&path[0]), nil
}

// Fingerprint is independent of inode, change time, access time, allocation,
// compression and document tracking, which need not survive a complete copy.
// MetadataSHA256 includes resource-fork contents as well as native metadata.
type Fingerprint struct {
	SHA256         string `json:"sha256"`
	MetadataSHA256 string `json:"metadata_sha256"`
}

func stat(f *os.File) (syscall.Stat_t, error) {
	defer runtime.KeepAlive(f)
	var st syscall.Stat_t
	err := syscall.Fstat(int(f.Fd()), &st)
	return st, err
}

func stable(a, b syscall.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode &&
		a.Nlink == b.Nlink && a.Uid == b.Uid && a.Gid == b.Gid && a.Size == b.Size &&
		a.Mtimespec == b.Mtimespec && a.Ctimespec == b.Ctimespec &&
		a.Birthtimespec == b.Birthtimespec && a.Flags == b.Flags
}

func number(h hash.Hash, n uint64) {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], n)
	h.Write(data[:])
}

func bytesField(h hash.Hash, b []byte) {
	number(h, uint64(len(b)))
	h.Write(b)
}

// FingerprintFile hashes complete regular-file contents and metadata using
// bounded reads, without changing f's offset. It rejects changes observed
// during verification. step receives cumulative primary-content bytes.
func FingerprintFile(ctx context.Context, f *os.File, step func(int64)) (Fingerprint, error) {
	if err := ctx.Err(); err != nil {
		return Fingerprint{}, err
	}
	before, err := stat(f)
	if err != nil {
		return Fingerprint{}, err
	}
	if before.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return Fingerprint{}, fmt.Errorf("%s is not a regular file", f.Name())
	}
	data := sha256.New()
	buffer := make([]byte, 1024*1024)
	var offset int64
	if step != nil {
		step(0)
	}
	for offset < before.Size {
		if err := ctx.Err(); err != nil {
			return Fingerprint{}, err
		}
		readSize := int64(len(buffer))
		if left := before.Size - offset; left < readSize {
			readSize = left
		}
		n, readErr := f.ReadAt(buffer[:readSize], offset)
		data.Write(buffer[:n])
		offset += int64(n)
		if step != nil {
			step(offset)
		}
		if readErr != nil && !(readErr == io.EOF && offset == before.Size) {
			return Fingerprint{}, fmt.Errorf("%w: reading %s: %v", ErrChanged, f.Name(), readErr)
		}
		if n == 0 && offset < before.Size {
			return Fingerprint{}, fmt.Errorf("%w: short read from %s", ErrChanged, f.Name())
		}
	}
	metadata := sha256.New()
	bytesField(metadata, []byte("spacehound metadata v1"))
	for _, n := range []uint64{
		uint64(before.Mode & 0o7777), uint64(before.Uid), uint64(before.Gid),
		uint64(before.Mtimespec.Sec), uint64(before.Mtimespec.Nsec),
		uint64(before.Birthtimespec.Sec), uint64(before.Birthtimespec.Nsec),
		uint64(before.Flags & uint32(C.sh_preservable_flags())),
	} {
		number(metadata, n)
	}
	if err := hashXattrs(ctx, f, metadata, buffer); err != nil {
		return Fingerprint{}, err
	}
	if err := hashACL(f, metadata); err != nil {
		return Fingerprint{}, err
	}
	after, err := stat(f)
	if err != nil {
		return Fingerprint{}, err
	}
	if !stable(before, after) {
		return Fingerprint{}, fmt.Errorf("%w: %s", ErrChanged, f.Name())
	}
	if err := ctx.Err(); err != nil {
		return Fingerprint{}, err
	}
	return Fingerprint{SHA256: hex.EncodeToString(data.Sum(nil)), MetadataSHA256: hex.EncodeToString(metadata.Sum(nil))}, nil
}

func hashACL(f *os.File, h hash.Hash) error {
	defer runtime.KeepAlive(f)
	var buf unsafe.Pointer
	var size C.ssize_t
	if err := errnoError("read ACL", f.Name(), C.sh_acl(C.int(f.Fd()), &buf, &size)); err != nil {
		return err
	}
	defer C.free(buf)
	// A native ACL cannot grow this large; refuse corrupt or unexpected results.
	if size < 0 || size > 16*1024*1024 {
		return fmt.Errorf("invalid ACL size for %s", f.Name())
	}
	bytesField(h, C.GoBytes(buf, C.int(size)))
	return nil
}

func hashXattrs(ctx context.Context, f *os.File, h hash.Hash, buffer []byte) error {
	defer runtime.KeepAlive(f)
	var size C.ssize_t
	if err := errnoError("list attributes", f.Name(), C.sh_xattr_names(C.int(f.Fd()), nil, 0, &size)); err != nil {
		return err
	}
	if size < 0 || size > 16*1024*1024 {
		return fmt.Errorf("invalid attribute list size for %s", f.Name())
	}
	var names []string
	if size > 0 {
		buf := make([]byte, int(size))
		var got C.ssize_t
		if err := errnoError("list attributes", f.Name(), C.sh_xattr_names(C.int(f.Fd()), (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(size), &got)); err != nil {
			return err
		}
		if got != size || buf[len(buf)-1] != 0 {
			return fmt.Errorf("%w: attribute names for %s", ErrChanged, f.Name())
		}
		names = strings.Split(string(buf[:len(buf)-1]), "\x00")
		sort.Strings(names)
	}
	number(h, uint64(len(names)))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "" {
			return fmt.Errorf("invalid empty attribute name for %s", f.Name())
		}
		bytesField(h, []byte(name))
		if err := hashXattr(ctx, f, name, h, buffer); err != nil {
			return err
		}
	}
	return nil
}

func hashXattr(ctx context.Context, f *os.File, name string, h hash.Hash, buffer []byte) error {
	defer runtime.KeepAlive(f)
	n := C.CString(name)
	defer C.free(unsafe.Pointer(n))
	var size C.ssize_t
	if err := errnoError("read attribute size", f.Name(), C.sh_xattr(C.int(f.Fd()), n, nil, 0, 0, &size)); err != nil {
		return err
	}
	// fgetxattr's resource-fork offset is only 32 bits. Refuse a fork that
	// cannot be completely verified rather than truncate or allocate it all.
	if size < 0 || uint64(size) > uint64(^uint32(0)) {
		return fmt.Errorf("attribute %q on %s exceeds verification limits", name, f.Name())
	}
	number(h, uint64(size))
	if size == 0 {
		return nil
	}
	if name != "com.apple.ResourceFork" {
		// Ordinary attributes do not support offsets and need a complete
		// buffer. Resource forks, potentially large, are streamed below.
		if size > 64*1024*1024 {
			return fmt.Errorf("attribute %q on %s exceeds verification limits", name, f.Name())
		}
		if int64(size) > int64(len(buffer)) {
			buffer = make([]byte, int(size))
		}
		buffer = buffer[:int(size)]
		var got C.ssize_t
		if err := errnoError("read attribute", f.Name(), C.sh_xattr(C.int(f.Fd()), n, unsafe.Pointer(&buffer[0]), C.size_t(len(buffer)), 0, &got)); err != nil {
			return err
		}
		if got != size {
			return fmt.Errorf("%w: attribute %q on %s", ErrChanged, name, f.Name())
		}
		h.Write(buffer)
		return nil
	}
	for offset := int64(0); offset < int64(size); {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := int64(len(buffer))
		if remaining := int64(size) - offset; remaining < chunk {
			chunk = remaining
		}
		var got C.ssize_t
		if err := errnoError("read resource fork", f.Name(), C.sh_xattr(C.int(f.Fd()), n, unsafe.Pointer(&buffer[0]), C.size_t(chunk), C.uint32_t(offset), &got)); err != nil {
			return err
		}
		if int64(got) != chunk {
			return fmt.Errorf("%w: resource fork on %s", ErrChanged, f.Name())
		}
		h.Write(buffer[:int(got)])
		offset += int64(got)
	}
	return nil
}

func linkAt(dir *os.File, from, to string) error {
	defer runtime.KeepAlive(dir)
	f, t := C.CString(from), C.CString(to)
	defer C.free(unsafe.Pointer(f))
	defer C.free(unsafe.Pointer(t))
	return errnoError("linkat", from+" -> "+to, C.sh_link(C.int(dir.Fd()), f, t))
}

// CheckGuardedUnlink proves protection on the actual source filesystem using
// files in a private generated directory. A kernel accepting but ignoring the
// flags fails this probe. It checks open descriptors, writable mappings after
// closing the descriptor, hard links, and successful deletion after closing.
func CheckGuardedUnlink(dir *os.File) (err error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	name := ".spacehound-probe-" + hex.EncodeToString(random[:])
	if err := MkdirAt(dir, name, 0o700); err != nil {
		return fmt.Errorf("%w: %v", ErrGuardedUnlinkUnsupported, err)
	}
	probe, openErr := OpenAt(dir, name, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if openErr != nil {
		RemoveDir(dir, name)
		return fmt.Errorf("%w: %v", ErrGuardedUnlinkUnsupported, openErr)
	}
	defer func() {
		for _, child := range []string{"file", "link"} {
			if removeErr := Unlink(probe, child, false); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
		if closeErr := probe.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		if removeErr := RemoveDir(dir, name); removeErr != nil {
			err = errors.Join(err, removeErr)
		}
	}()
	if err := MakePrivate(probe, 0o700); err != nil {
		return fmt.Errorf("%w: %v", ErrGuardedUnlinkUnsupported, err)
	}
	if probeErr := probeGuarded(probe); probeErr != nil {
		return fmt.Errorf("%w: %v", ErrGuardedUnlinkUnsupported, probeErr)
	}
	return nil
}

func probeGuarded(dir *os.File) error {
	f, err := OpenAt(dir, "file", syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(int64(os.Getpagesize())); err != nil {
		return err
	}
	if err := Unlink(dir, "file", true); !errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("open-descriptor protection failed: %v", err)
	}
	mapping, err := syscall.Mmap(int(f.Fd()), 0, os.Getpagesize(), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("test writable mapping: %w", err)
	}
	if err := f.Close(); err != nil {
		syscall.Munmap(mapping)
		return err
	}
	busyErr := Unlink(dir, "file", true)
	unmapErr := syscall.Munmap(mapping)
	if !errors.Is(busyErr, syscall.EBUSY) {
		return fmt.Errorf("writable-mapping protection failed: %v", busyErr)
	}
	if unmapErr != nil {
		return unmapErr
	}
	if err := linkAt(dir, "file", "link"); err != nil {
		return err
	}
	if err := Unlink(dir, "file", true); !errors.Is(err, syscall.Errno(C.ENOTCAPABLE)) {
		return fmt.Errorf("hard-link protection failed: %v", err)
	}
	if err := Unlink(dir, "link", false); err != nil {
		return err
	}
	if err := Unlink(dir, "file", true); err != nil {
		return fmt.Errorf("closed single-link file cannot be removed: %w", err)
	}
	return nil
}
