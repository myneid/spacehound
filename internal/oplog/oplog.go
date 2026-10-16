// Package oplog appends a JSON-lines record of every mutating operation to
// ~/Library/Logs/SpaceHound/operations.log. It records paths, sizes,
// hashes and outcomes, never file contents.
package oplog

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"spacehound/internal/fsid"
	"spacehound/internal/macosfs"
)

// FileName is the log file inside the log directory.
const FileName = "operations.log"

const maxRecordBytes = 1024 * 1024

// DefaultDir returns ~/Library/Logs/SpaceHound.
func DefaultDir(home string) string { return filepath.Join(home, "Library", "Logs", "SpaceHound") }

// Entry is one logged operation.
type Entry struct {
	ID           string    `json:"operation_id,omitempty"`
	Phase        string    `json:"phase,omitempty"` // intent, captured, complete, recovery
	RecoveryPath string    `json:"recovery_path,omitempty"`
	Time         time.Time `json:"time"`
	Action       string    `json:"action"` // trash, offload-copy, offload-remove-source, offload-trash-source
	Path         string    `json:"path"`
	Dest         string    `json:"dest,omitempty"`
	Size         int64     `json:"size,omitempty"`
	SHA256       string    `json:"sha256,omitempty"`
	OK           bool      `json:"ok"`
	Error        string    `json:"error,omitempty"`
	FreeBefore   int64     `json:"internal_free_before,omitempty"`
	FreeAfter    int64     `json:"internal_free_after,omitempty"`
}

// Logger writes entries. Log on a nil *Logger discards entries; Check and
// Pending require a logger.
type Logger struct {
	Dir string
	Now func() time.Time
	mu  sync.Mutex

	cache journalCache
	// readRecord observes deserialization for regression tests. It is called
	// under mu, and is never needed by production callers.
	readRecord func()
	// beforeAppend permits a deterministic outside-writer regression test.
	beforeAppend func()
}

type journalCache struct {
	valid   bool
	id      fsid.Identity
	digest  [sha256.Size]byte
	pending map[string]Entry
}

// New returns a logger writing into dir. The directory is created lazily
// on the first write.
func New(dir string) *Logger { return &Logger{Dir: dir} }

// Path returns the log file path.
func (l *Logger) Path() string { return filepath.Join(l.Dir, FileName) }

// Log appends and syncs e. The log is private to the user (0600).
func (l *Logger) Log(e Entry) (err error) {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	defer wrapError(&err)
	if err := validateEntry(e); err != nil {
		return err
	}
	if e.Time.IsZero() {
		if l.Now != nil {
			e.Time = l.Now()
		} else {
			e.Time = time.Now()
		}
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b)+1 > maxRecordBytes {
		return errors.New("record exceeds 1 MiB limit")
	}
	dir, f, err := l.open(true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeJournal(f, dir)) }()
	if err := l.replay(f); err != nil {
		return err
	}
	before := l.cache.id
	if l.beforeAppend != nil {
		l.beforeAppend()
	}
	b = append(b, '\n')
	if n, err := f.Write(b); err != nil {
		return err
	} else if n != len(b) {
		return io.ErrShortWrite
	}
	if err := syncFiles(f, dir); err != nil {
		return err
	}
	after, _, err := fsid.FromOpen(f)
	if err != nil {
		return err
	}
	if !after.SameFile(before) || after.Size != before.Size+int64(len(b)) {
		return errors.New("journal changed during append")
	}
	// An overlapping outside write can share the timestamps produced by our
	// append. Rehash the old prefix before assigning those timestamps to cached
	// state; the advisory lock alone cannot exclude noncooperating writers.
	h := sha256.New()
	n, err := io.Copy(h, io.NewSectionReader(f, 0, before.Size))
	if err != nil {
		return err
	}
	if n != before.Size {
		return io.ErrUnexpectedEOF
	}
	if !bytes.Equal(h.Sum(nil), l.cache.digest[:]) {
		return errors.New("journal history changed during append")
	}
	tail := make([]byte, len(b))
	if _, err := f.ReadAt(tail, before.Size); err != nil {
		return err
	}
	if !bytes.Equal(tail, b) {
		return errors.New("journal appended record changed")
	}
	if err := validateCurrent(dir, f, after); err != nil {
		return err
	}
	h.Write(b)
	applyEntry(l.cache.pending, e)
	l.cache.id = after
	copy(l.cache.digest[:], h.Sum(nil))
	return nil
}

// Check prepares the journal and validates it before a destructive operation.
// Unlike Log, Check rejects a nil logger: a live mutation needs a journal.
func (l *Logger) Check() (err error) {
	if l == nil {
		return errors.New("operation log: logger is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	defer wrapError(&err)
	dir, f, err := l.open(true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeJournal(f, dir)) }()
	if err := l.replay(f); err != nil {
		return err
	}
	if err := syncFiles(f, dir); err != nil {
		return err
	}
	return validateCurrent(dir, f, l.cache.id)
}

// Pending returns each operation whose latest phase is not complete. It does
// not create or alter the journal. Invalid or truncated records are errors so
// an uncertain recovery state is never silently presented as an empty journal.
func (l *Logger) Pending() (pending []Entry, err error) {
	if l == nil {
		return nil, errors.New("operation log: logger is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	defer wrapError(&err)
	dir, f, err := l.open(false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if l.cache.valid {
				return nil, errors.New("previously validated journal disappeared")
			}
			return nil, nil
		}
		return nil, err
	}
	defer func() { err = errors.Join(err, closeJournal(f, dir)) }()
	if err := l.replay(f); err != nil {
		return nil, err
	}
	if err := validateCurrent(dir, f, l.cache.id); err != nil {
		return nil, err
	}
	for _, e := range l.cache.pending {
		pending = append(pending, e)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	return pending, nil
}

// replay reuses only an exactly matching, previously validated file. A larger
// size alone is not evidence of an append: outside changes must replay the
// entire journal, including any rewritten prefix. Our own durable appends can
// update the pending map directly while holding the exclusive file lock.
func (l *Logger) replay(f *os.File) error {
	id, _, err := fsid.FromOpen(f)
	if err != nil {
		return err
	}
	if l.cache.valid {
		if id == l.cache.id {
			return nil
		}
		if id.SameFile(l.cache.id) && id.Size < l.cache.id.Size {
			return errors.New("previously validated journal was truncated")
		}
	}
	byID := make(map[string]Entry)
	h := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(io.NewSectionReader(f, 0, id.Size), h))
	scanner.Buffer(make([]byte, 4096), maxRecordBytes)
	scanner.Split(journalLines)
	line := 0
	for scanner.Scan() {
		line++
		if l.readRecord != nil {
			l.readRecord()
		}
		var e *Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return fmt.Errorf("record %d: %w", line, err)
		}
		if e == nil {
			return fmt.Errorf("record %d: expected an operation object", line)
		}
		if err := validateEntry(*e); err != nil {
			return fmt.Errorf("record %d: %w", line, err)
		}
		applyEntry(byID, *e)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("record %d: %w", line+1, err)
	}
	after, _, err := fsid.FromOpen(f)
	if err != nil {
		return err
	}
	if after != id {
		return errors.New("journal changed during validation")
	}
	l.cache = journalCache{valid: true, id: id, pending: byID}
	copy(l.cache.digest[:], h.Sum(nil))
	return nil
}

func applyEntry(byID map[string]Entry, e Entry) {
	if e.Phase == "complete" {
		delete(byID, e.ID)
	} else if e.Phase != "" {
		byID[e.ID] = e
	}
}

func validateCurrent(dir, f *os.File, want fsid.Identity) (err error) {
	if err := validateFile(f, true); err != nil {
		return err
	}
	current, err := macosfs.OpenAt(dir, FileName, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("reopen journal during validation: %w", err)
	}
	defer func() { err = errors.Join(err, current.Close()) }()
	id, _, err := fsid.FromOpen(current)
	if err != nil {
		return err
	}
	after, _, err := fsid.FromOpen(f)
	if err != nil {
		return err
	}
	if id != want || after != want {
		return errors.New("journal changed during operation")
	}
	return nil
}

func (l *Logger) open(writable bool) (*os.File, *os.File, error) {
	if l.Dir == "" {
		return nil, nil, errors.New("empty log directory")
	}
	path, err := filepath.Abs(l.Dir)
	if err != nil {
		return nil, nil, err
	}
	dir, err := macosfs.OpenDir(path, writable && !l.cache.valid, 0o700)
	if err != nil {
		return nil, nil, err
	}
	lock := syscall.LOCK_SH
	if writable {
		lock = syscall.LOCK_EX
	}
	// Lock the stable directory before opening the file as well: this
	// serializes first creation and pathname changes between logger instances.
	// Always acquire directory then file locks, and release in reverse order.
	if err := syscall.Flock(int(dir.Fd()), lock); err != nil {
		return nil, nil, errors.Join(err, dir.Close())
	}
	fail := func(err error) (*os.File, *os.File, error) {
		return nil, nil, errors.Join(err, closeDirectory(dir))
	}
	if err := validateFile(dir, false); err != nil {
		return fail(err)
	}
	if writable {
		if err := macosfs.MakePrivate(dir, 0o700); err != nil {
			return fail(err)
		}
	}
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if writable {
		flags = os.O_RDWR | os.O_APPEND | syscall.O_NONBLOCK
		if !l.cache.valid {
			flags |= os.O_CREATE
		}
	}
	f, err := macosfs.OpenAt(dir, FileName, flags, 0o600)
	if err != nil {
		return fail(fmt.Errorf("open journal: %w", err))
	}
	if err := syscall.Flock(int(f.Fd()), lock); err != nil {
		return nil, nil, errors.Join(err, f.Close(), closeDirectory(dir))
	}
	if err := validateFile(f, true); err != nil {
		return nil, nil, errors.Join(err, closeJournal(f, dir))
	}
	if writable {
		if err := macosfs.MakePrivate(f, 0o600); err != nil {
			return nil, nil, errors.Join(err, closeJournal(f, dir))
		}
	}
	return dir, f, nil
}

func validateFile(f *os.File, regular bool) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot validate %s ownership", f.Name())
	}
	if st.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s is owned by another user", f.Name())
	}
	if regular {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", f.Name())
		}
		if st.Nlink != 1 {
			return fmt.Errorf("%s has %d hard links; require exactly one", f.Name(), st.Nlink)
		}
	} else if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", f.Name())
	}
	return nil
}

func validateEntry(e Entry) error {
	if e.Phase == "" {
		return nil
	}
	if e.ID == "" || e.Path == "" {
		return errors.New("phased record needs an operation ID and source path")
	}
	switch e.Phase {
	case "intent", "captured", "recovery":
		if e.RecoveryPath == "" {
			return fmt.Errorf("%s record needs a recovery path", e.Phase)
		}
	case "complete":
	default:
		return fmt.Errorf("unknown phase %q", e.Phase)
	}
	return nil
}

func journalLines(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, errors.New("truncated journal record (missing newline)")
	}
	return 0, nil, nil
}

func syncFiles(files ...*os.File) error {
	for _, f := range files {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	return nil
}

func closeDirectory(dir *os.File) error {
	return errors.Join(syscall.Flock(int(dir.Fd()), syscall.LOCK_UN), dir.Close())
}

func closeJournal(f, dir *os.File) error {
	return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close(), closeDirectory(dir))
}

func wrapError(err *error) {
	if *err != nil {
		*err = fmt.Errorf("operation log: %w", *err)
	}
}
