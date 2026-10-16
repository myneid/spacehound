package fileops

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"spacehound/internal/fsid"
	"spacehound/internal/macosfs"
	"spacehound/internal/oplog"
)

func TestTakeAndRestoreCancelledOperation(t *testing.T) {
	f := captureFixture(t)
	c, err := Take(f.source, f.identity, f.journal, "trash", "/unused-destination")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	assertCaptureAbsent(t, f.source)
	assertCaptureContents(t, c.RecoveryPath(), f.contents)
	id, _, err := fsid.FromOpen(c.File)
	if err != nil || !id.SameFile(f.identity) {
		t.Fatalf("capture did not retain original inode: %+v, %v", id, err)
	}
	pending := pendingCaptures(t, f.journal)
	if len(pending) != 1 || pending[0].ID != c.Entry.ID || pending[0].Phase != "captured" || pending[0].RecoveryPath != c.RecoveryPath() {
		t.Fatalf("missing capture recovery record: %+v", pending)
	}
	if err := c.Restore(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("restore lost cancellation cause: %v", err)
	}
	assertCaptureContents(t, f.source, f.contents)
	assertCaptureAbsent(t, c.RecoveryPath())
	if pending := pendingCaptures(t, f.journal); len(pending) != 0 {
		t.Fatalf("restored operation remains unresolved: %+v", pending)
	}
	last := lastCaptureRecord(t, f.journal)
	if last.Phase != "complete" || last.OK || last.Error == "" || last.RecoveryPath != "" {
		t.Fatalf("restoration did not record failed-operation completion: %+v", last)
	}
	c.Close()
	assertNoHoldingDirectories(t, filepath.Dir(f.source))
}

func TestRestorePreservesOriginalWhenSourceNameIsOccupied(t *testing.T) {
	f := captureFixture(t)
	c, err := Take(f.source, f.identity, f.journal, "offload-remove-source", "/unused-archive")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	recoveryPath := c.RecoveryPath()
	replacement := "new data at original source name"
	if err := os.WriteFile(f.source, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	err = c.Restore(context.Canceled)
	var recovery *RecoveryError
	if !errors.As(err, &recovery) || recovery.RecoveryPath != recoveryPath || !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked restoration did not report recovery and original cause: %v", err)
	}
	assertCaptureContents(t, f.source, replacement)
	assertCaptureContents(t, recoveryPath, f.contents)
	pending := pendingCaptures(t, f.journal)
	if len(pending) != 1 || pending[0].Phase != "recovery" || pending[0].RecoveryPath != recoveryPath || pending[0].Path != f.source {
		t.Fatalf("blocked original is missing from recovery journal: %+v", pending)
	}
	c.Close()
	assertCaptureContents(t, recoveryPath, f.contents)
	assertCaptureContents(t, f.source, replacement)
}

func TestRestoreRetainsRecoveryWhenSourceParentWasReplaced(t *testing.T) {
	f := captureFixture(t)
	c, err := Take(f.source, f.identity, f.journal, "trash", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	originalParent := filepath.Dir(f.source)
	relocated := filepath.Join(f.root, "relocated-source")
	if err := os.Rename(originalParent, relocated); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(originalParent, 0o700); err != nil {
		t.Fatal(err)
	}
	recoveryPath := filepath.Join(relocated, c.DirName, "original")
	var recovery *RecoveryError
	if err := c.Restore(context.Canceled); !errors.As(err, &recovery) || recovery.RecoveryPath != recoveryPath {
		t.Fatalf("relocated source parent did not retain discoverable recovery: %v", err)
	}
	assertCaptureAbsent(t, f.source)
	assertCaptureAbsent(t, filepath.Join(relocated, filepath.Base(f.source)))
	assertCaptureContents(t, recoveryPath, f.contents)
	pending := pendingCaptures(t, f.journal)
	if len(pending) != 1 || pending[0].Phase != "recovery" || pending[0].RecoveryPath != recoveryPath {
		t.Fatalf("relocated recovery path was not recorded: %+v", pending)
	}
	c.Close()
	assertCaptureContents(t, recoveryPath, f.contents)
}

func TestBlockedRestoreWithNilCauseReportsRecovery(t *testing.T) {
	f := captureFixture(t)
	c, err := Take(f.source, f.identity, f.journal, "trash", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if err := os.WriteFile(f.source, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	var recovery *RecoveryError
	if err := c.Restore(nil); !errors.As(err, &recovery) {
		t.Fatalf("blocked restore with no cause did not report recovery: %v", err)
	}
	assertCaptureContents(t, c.RecoveryPath(), f.contents)
	assertCaptureContents(t, f.source, "replacement")
}

func TestDoneAfterNativeMoveOrUnlink(t *testing.T) {
	for _, operation := range []string{"move", "unlink"} {
		t.Run(operation, func(t *testing.T) {
			f := captureFixture(t)
			c, err := Take(f.source, f.identity, f.journal, operation, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(c.Close)
			var moved string
			if operation == "move" {
				dir, err := macosfs.OpenDir(f.root, false, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer dir.Close()
				if err := macosfs.Rename(c.Dir, "original", dir, "moved.bin"); err != nil {
					t.Fatal(err)
				}
				moved = filepath.Join(f.root, "moved.bin")
				if err := dir.Sync(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := c.File.Close(); err != nil {
					t.Fatal(err)
				}
				c.File = nil
				// This tests journal completion for a disposable fixture. Guarded
				// unlink support and protection are tested in macosfs.
				if err := macosfs.Unlink(c.Dir, "original", false); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.Dir.Sync(); err != nil {
				t.Fatal(err)
			}
			if err := c.Done(); err != nil {
				t.Fatal(err)
			}
			assertCaptureAbsent(t, f.source)
			assertCaptureAbsent(t, c.RecoveryPath())
			if moved != "" {
				assertCaptureContents(t, moved, f.contents)
				id, _, err := fsid.Of(moved)
				if err != nil || !id.SameFile(f.identity) {
					t.Fatalf("move changed original identity: %+v, %v", id, err)
				}
			}
			if pending := pendingCaptures(t, f.journal); len(pending) != 0 {
				t.Fatalf("completed operation remains unresolved: %+v", pending)
			}
			last := lastCaptureRecord(t, f.journal)
			if last.Phase != "complete" || !last.OK || last.RecoveryPath != "" || last.ID != c.Entry.ID {
				t.Fatalf("incorrect completion record: %+v", last)
			}
			c.Close()
			assertNoHoldingDirectories(t, filepath.Dir(f.source))
		})
	}
}

func TestTakeRequiresAvailableJournalBeforeMovingSource(t *testing.T) {
	for _, unavailable := range []string{"nil", "directory-is-file", "corrupt"} {
		t.Run(unavailable, func(t *testing.T) {
			f := captureFixture(t)
			var journal *oplog.Logger
			switch unavailable {
			case "directory-is-file":
				journal = oplog.New(f.source)
			case "corrupt":
				journal = f.journal
				if err := os.MkdirAll(journal.Dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(journal.Path(), []byte("unfinished journal"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if c, err := Take(f.source, f.identity, journal, "trash", ""); err == nil || c != nil {
				if c != nil {
					c.Close()
				}
				t.Fatalf("capture accepted unavailable journal: %v", err)
			}
			assertCaptureContents(t, f.source, f.contents)
			id, _, err := fsid.Of(f.source)
			if err != nil || !id.SameFile(f.identity) {
				t.Fatalf("journal failure changed source identity: %+v, %v", id, err)
			}
			assertNoHoldingDirectories(t, filepath.Dir(f.source))
		})
	}
}

func TestTakeRejectsStaleIdentityWithoutMovingSource(t *testing.T) {
	f := captureFixture(t)
	stale := f.identity
	stale.Ino++
	if c, err := Take(f.source, stale, f.journal, "trash", ""); !errors.Is(err, fsid.ErrChanged) || c != nil {
		if c != nil {
			c.Close()
		}
		t.Fatalf("stale identity was not refused: %v", err)
	}
	assertCaptureContents(t, f.source, f.contents)
	assertNoHoldingDirectories(t, filepath.Dir(f.source))
	if pending := pendingCaptures(t, f.journal); len(pending) != 0 {
		t.Fatalf("refused capture left an unresolved intent: %+v", pending)
	}
}

func TestInterruptedCaptureRemainsDiscoverableAfterClosing(t *testing.T) {
	f := captureFixture(t)
	c, err := Take(f.source, f.identity, f.journal, "offload-remove-source", "/unused-archive")
	if err != nil {
		t.Fatal(err)
	}
	recoveryPath := c.RecoveryPath()
	operationID := c.Entry.ID
	c.Close() // Simulate process exit before either Restore or Done.
	for i := 0; i < 2; i++ {
		pending := pendingCaptures(t, oplog.New(f.journal.Dir))
		if len(pending) != 1 || pending[0].ID != operationID || pending[0].Phase != "captured" || pending[0].RecoveryPath != recoveryPath {
			t.Fatalf("interrupted capture became undiscoverable: %+v", pending)
		}
		assertCaptureContents(t, recoveryPath, f.contents)
		assertCaptureAbsent(t, f.source)
	}
}

func TestInterruptedIntentPreservesBeforeAndAfterMoveState(t *testing.T) {
	for _, move := range []bool{false, true} {
		name := "before-move"
		if move {
			name = "after-move"
		}
		t.Run(name, func(t *testing.T) {
			f := captureFixture(t)
			holdPath := filepath.Join(filepath.Dir(f.source), ".spacehound-hold-interrupted")
			if err := os.Mkdir(holdPath, 0o700); err != nil {
				t.Fatal(err)
			}
			recoveryPath := filepath.Join(holdPath, "original")
			entry := oplog.Entry{ID: ID(), Phase: "intent", Action: "trash", Path: f.source, RecoveryPath: recoveryPath}
			if err := f.journal.Log(entry); err != nil {
				t.Fatal(err)
			}
			if move {
				parent, err := macosfs.OpenDir(filepath.Dir(f.source), false, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer parent.Close()
				hold, err := macosfs.OpenDir(holdPath, false, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer hold.Close()
				if err := macosfs.Rename(parent, filepath.Base(f.source), hold, "original"); err != nil {
					t.Fatal(err)
				}
			}
			pending := pendingCaptures(t, oplog.New(f.journal.Dir))
			if len(pending) != 1 || pending[0].ID != entry.ID || pending[0].Phase != "intent" || pending[0].RecoveryPath != recoveryPath {
				t.Fatalf("intent lost across process reopen: %+v", pending)
			}
			if move {
				assertCaptureAbsent(t, f.source)
				assertCaptureContents(t, recoveryPath, f.contents)
			} else {
				assertCaptureContents(t, f.source, f.contents)
				assertCaptureAbsent(t, recoveryPath)
				if _, err := os.Stat(holdPath); err != nil {
					t.Fatalf("reading recovery journal removed the holding directory: %v", err)
				}
			}
		})
	}
}

type captureTestFixture struct {
	root, source, contents string
	identity               fsid.Identity
	journal                *oplog.Logger
}

func captureFixture(t *testing.T) captureTestFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "source")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	f := captureTestFixture{root: root, source: filepath.Join(parent, "valuable.bin"), contents: "the only original contents", journal: oplog.New(filepath.Join(root, "logs", "SpaceHound"))}
	if err := os.WriteFile(f.source, []byte(f.contents), 0o600); err != nil {
		t.Fatal(err)
	}
	f.identity, _, err = fsid.Of(f.source)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func pendingCaptures(t *testing.T, journal *oplog.Logger) []oplog.Entry {
	t.Helper()
	entries, err := journal.Pending()
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func lastCaptureRecord(t *testing.T, journal *oplog.Logger) oplog.Entry {
	t.Helper()
	f, err := os.Open(journal.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var last oplog.Entry
	count := 0
	for scanner.Scan() {
		var entry oplog.Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		last = entry
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("missing operation journal")
	}
	return last
}

func assertCaptureContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s contains %q, want %q", path, got, want)
	}
}

func assertCaptureAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s absent, got %v", path, err)
	}
}

func assertNoHoldingDirectories(t *testing.T, parent string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(parent, ".spacehound-hold-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Errorf("operation left unexpected holding directories: %v", paths)
	}
}
