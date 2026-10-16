package oplog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestLogAppendsJSONLines(t *testing.T) {
	dir := filepath.Join(fixtureDir(t), "Logs", "SpaceHound")
	l := New(dir)
	if err := l.Log(Entry{Action: "trash", Path: "/x/a", Size: 3, OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := l.Log(Entry{Action: "offload-copy", Path: "/x/b", Dest: "/Volumes/D/b", Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode %v, want 0600", fi.Mode().Perm())
	}
	f, _ := os.Open(l.Path())
	defer f.Close()
	sc := bufio.NewScanner(f)
	var n int
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.Time.IsZero() {
			t.Error("missing timestamp")
		}
		n++
	}
	if n != 2 {
		t.Fatalf("got %d lines", n)
	}
	var nilLogger *Logger
	if err := nilLogger.Log(Entry{}); err != nil {
		t.Error("nil logger should discard")
	}
}

func TestDefaultDir(t *testing.T) {
	if got := DefaultDir("/Users/me"); got != "/Users/me/Library/Logs/SpaceHound" {
		t.Error(got)
	}
}

func TestCheckTightensOnlyJournalPermissions(t *testing.T) {
	parent := filepath.Join(fixtureDir(t), "Logs")
	dir := filepath.Join(parent, "SpaceHound")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	l := New(dir)
	if err := os.WriteFile(l.Path(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{parent: 0o755, dir: 0o700, l.Path(): 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", path, got, want)
		}
	}
}

func TestCheckClearsInheritedACLGrants(t *testing.T) {
	parent := fixtureDir(t)
	grant := "everyone allow read,execute,readattr,readextattr,readsecurity,file_inherit,directory_inherit"
	if out, err := exec.Command("/bin/chmod", "+a", grant, parent).CombinedOutput(); err != nil {
		t.Fatalf("set fixture ACL: %v: %s", err, out)
	}
	l := New(filepath.Join(parent, "SpaceHound"))
	if err := os.Mkdir(l.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.Path(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{l.Dir, l.Path()} {
		if !strings.Contains(aclListing(t, path), "inherited allow") {
			t.Fatalf("fixture %s did not inherit the granting ACL", path)
		}
	}
	if err := l.Check(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{l.Dir, l.Path()} {
		if listing := aclListing(t, path); strings.Contains(listing, "everyone") {
			t.Errorf("journal ACL still grants access to everyone: %s", listing)
		}
	}
	if !strings.Contains(aclListing(t, parent), "everyone") {
		t.Error("Check altered a generic parent directory's ACL")
	}
}

func TestJournalRejectsSymlinks(t *testing.T) {
	for _, where := range []string{"ancestor", "directory", "file"} {
		t.Run(where, func(t *testing.T) {
			base := fixtureDir(t)
			target := filepath.Join(base, "target")
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(base, "SpaceHound")
			switch where {
			case "ancestor":
				link := filepath.Join(base, "parent")
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				dir = filepath.Join(link, "SpaceHound")
			case "directory":
				if err := os.Symlink(target, dir); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(target, "existing.log")
				if err := os.WriteFile(target, []byte("untouched\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(dir, FileName)); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			l := New(dir)
			if err := l.Check(); err == nil {
				t.Error("Check followed a symlink")
			}
			if err := l.Log(Entry{Action: "trash", Path: "/fixture"}); err == nil {
				t.Error("Log followed a symlink")
			}
			if _, err := l.Pending(); err == nil {
				t.Error("Pending followed a symlink")
			}
			after, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if before.Mode() != after.Mode() || before.Size() != after.Size() {
				t.Error("symlink target was changed")
			}
			if where == "file" {
				assertContents(t, target, []byte("untouched\n"))
			}
		})
	}
}

func TestJournalRejectsHardLinks(t *testing.T) {
	base := fixtureDir(t)
	target := filepath.Join(base, "existing.log")
	if err := os.WriteFile(target, []byte("untouched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "SpaceHound")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	l := New(dir)
	if err := os.Link(target, l.Path()); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); err == nil {
		t.Error("Check accepted hard-linked log")
	}
	if err := l.Log(Entry{Action: "trash", Path: "/fixture"}); err == nil {
		t.Error("Log accepted hard-linked log")
	}
	if _, err := l.Pending(); err == nil {
		t.Error("Pending accepted hard-linked log")
	}
	assertContents(t, target, []byte("untouched\n"))
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Error("hard-link target permissions were changed")
	}
}

func TestJournalRejectsNonregularFiles(t *testing.T) {
	for _, kind := range []string{"fifo", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := fixtureDir(t)
			l := New(dir)
			var err error
			if kind == "fifo" {
				err = syscall.Mkfifo(l.Path(), 0o600)
			} else {
				err = os.Mkdir(l.Path(), 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := l.Check(); err == nil {
				t.Error("Check accepted nonregular log")
			}
			if _, err := l.Pending(); err == nil {
				t.Error("Pending accepted nonregular log")
			}
		})
	}
}

func TestPendingLatestPhases(t *testing.T) {
	l := New(filepath.Join(fixtureDir(t), "SpaceHound"))
	l.Now = func() time.Time { return time.Unix(42, 0) }
	entries := []Entry{
		{Action: "trash", Path: "/old", OK: true}, // Legacy records do not imply recovery work.
		{ID: "a", Phase: "intent", Path: "/a", RecoveryPath: "/recovery/a"},
		{ID: "a", Phase: "captured", Path: "/a", RecoveryPath: "/recovery/a"},
		{ID: "b", Phase: "intent", Path: "/b", RecoveryPath: "/recovery/b"},
		{ID: "b", Phase: "complete", Path: "/b", OK: true},
		{ID: "c", Phase: "intent", Path: "/c", RecoveryPath: "/recovery/c"},
		{ID: "c", Phase: "recovery", Path: "/c", RecoveryPath: "/recovery/c", Error: "original path occupied"},
		{ID: "d", Phase: "intent", Path: "/d", RecoveryPath: "/recovery/d"},
	}
	for _, e := range entries {
		if err := l.Log(e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "a" || got[0].Phase != "captured" || got[1].ID != "c" || got[1].Phase != "recovery" || got[2].ID != "d" || got[2].Phase != "intent" {
		t.Fatalf("pending = %+v", got)
	}
	if !got[0].Time.Equal(l.Now()) || got[1].RecoveryPath != "/recovery/c" {
		t.Fatalf("lost recovery details: %+v", got)
	}
}

func TestPendingDoesNotCreateOrChangeFiles(t *testing.T) {
	base := fixtureDir(t)
	l := New(filepath.Join(base, "absent", "SpaceHound"))
	if got, err := l.Pending(); err != nil || len(got) != 0 {
		t.Fatalf("missing journal: %v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(base, "absent")); !os.IsNotExist(err) {
		t.Fatalf("Pending created directories: %v", err)
	}
	l = New(filepath.Join(base, "SpaceHound"))
	if err := os.Mkdir(l.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Pending(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.Path()); !os.IsNotExist(err) {
		t.Fatalf("Pending created log: %v", err)
	}
	b := []byte("{\"operation_id\":\"a\",\"phase\":\"intent\",\"path\":\"/a\",\"recovery_path\":\"/r/a\"}\n")
	if err := os.WriteFile(l.Path(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.Path(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Pending(); err != nil {
		t.Fatal(err)
	}
	assertContents(t, l.Path(), b)
	for path, want := range map[string]os.FileMode{l.Dir: 0o755, l.Path(): 0o644} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("Pending changed %s permissions", path)
		}
	}
}

func TestJournalRefusesUnknownRecoveryState(t *testing.T) {
	for name, contents := range map[string]string{
		"malformed":             "{bad}\n",
		"truncated":             "{\"action\":\"trash\"}",
		"null":                  "null\n",
		"blank":                 "\n",
		"unknown phase":         "{\"operation_id\":\"a\",\"phase\":\"unknown\",\"path\":\"/a\"}\n",
		"missing operation ID":  "{\"phase\":\"intent\",\"path\":\"/a\",\"recovery_path\":\"/r/a\"}\n",
		"missing recovery path": "{\"operation_id\":\"a\",\"phase\":\"captured\",\"path\":\"/a\"}\n",
		"oversized record":      "{\"path\":\"" + strings.Repeat("a", 1024*1024) + "\"}\n",
	} {
		t.Run(name, func(t *testing.T) {
			l := New(fixtureDir(t))
			if err := os.WriteFile(l.Path(), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := l.Pending(); err == nil {
				t.Error("Pending silently dropped invalid record")
			}
			if err := l.Check(); err == nil {
				t.Error("Check accepted an unreadable recovery state")
			}
			if err := l.Log(Entry{Action: "trash", Path: "/new"}); err == nil {
				t.Error("Log appended to an unreadable recovery state")
			}
			assertContents(t, l.Path(), []byte(contents))
		})
	}
}

func TestLiveJournalRequiresLogger(t *testing.T) {
	var l *Logger
	if err := l.Check(); err == nil {
		t.Error("nil preflight accepted")
	}
	if _, err := l.Pending(); err == nil {
		t.Error("nil recovery journal accepted")
	}
}

func TestLogRejectsOversizedEntryBeforeWriting(t *testing.T) {
	l := New(filepath.Join(fixtureDir(t), "SpaceHound"))
	if err := l.Log(Entry{Action: "trash", Path: "/fixture", Error: strings.Repeat("a", maxRecordBytes)}); err == nil {
		t.Fatal("accepted an entry that recovery parsing cannot read")
	}
	if _, err := os.Stat(l.Dir); !os.IsNotExist(err) {
		t.Fatalf("invalid entry created journal directory: %v", err)
	}
}

func TestJournalReusesValidatedHistoryAndAppliesOwnAppends(t *testing.T) {
	l := New(fixtureDir(t))
	var history bytes.Buffer
	const records = 1000
	for i := 0; i < records; i++ {
		history.Write(journalRecord(t, Entry{ID: fmt.Sprint(i), Phase: "intent", Path: "/fixture", RecoveryPath: "/recovery"}))
	}
	if err := os.WriteFile(l.Path(), history.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed := 0
	l.readRecord = func() { parsed++ }
	if err := l.Check(); err != nil {
		t.Fatal(err)
	}
	if parsed != records {
		t.Fatalf("initial validation deserialized %d records, want %d", parsed, records)
	}
	for i := 0; i < 10; i++ {
		if err := l.Check(); err != nil {
			t.Fatal(err)
		}
		if err := l.Log(Entry{ID: fmt.Sprint(i), Phase: "complete", Path: "/fixture"}); err != nil {
			t.Fatal(err)
		}
		pending, err := l.Pending()
		if err != nil || len(pending) != records-i-1 {
			t.Fatalf("pending after append %d: %d entries, %v", i, len(pending), err)
		}
	}
	if parsed != records {
		t.Fatalf("unchanged history was deserialized again: got %d, want %d", parsed, records)
	}
	// A new process/logger must recover from the durable journal, independently
	// of the map maintained by the writer.
	fresh := New(l.Dir)
	freshParsed := 0
	fresh.readRecord = func() { freshParsed++ }
	pending, err := fresh.Pending()
	if err != nil || len(pending) != records-10 || freshParsed != records+10 {
		t.Fatalf("fresh replay: %d pending, %d parsed, %v", len(pending), freshParsed, err)
	}
}

func TestJournalExternalAppendReplaysWholeHistory(t *testing.T) {
	l := New(fixtureDir(t))
	if err := l.Log(Entry{ID: "a", Phase: "intent", Path: "/a", RecoveryPath: "/r/a"}); err != nil {
		t.Fatal(err)
	}
	parsed := 0
	l.readRecord = func() { parsed++ }
	appendJournal(t, l.Path(), journalRecord(t, Entry{ID: "b", Phase: "intent", Path: "/b", RecoveryPath: "/r/b"}))
	pending, err := l.Pending()
	if err != nil || len(pending) != 2 || pending[1].ID != "b" || parsed != 2 {
		t.Fatalf("external append: %+v, %d parsed, %v", pending, parsed, err)
	}
	if err := l.Check(); err != nil || parsed != 2 {
		t.Fatalf("repeated validation: %d parsed, %v", parsed, err)
	}
	// Growth cannot be trusted as proof of append-only changes. A rewritten
	// historical prefix plus a valid appended record must also be rejected.
	f, err := os.OpenFile(l.Path(), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("!"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	appendJournal(t, l.Path(), journalRecord(t, Entry{Action: "trash", Path: "/legacy"}))
	assertJournalRefused(t, l)
}

func TestJournalRejectsPrefixRewriteOverlappingOwnAppend(t *testing.T) {
	for _, rewrite := range []string{"malformed", "valid JSON"} {
		t.Run(rewrite, func(t *testing.T) {
			l := New(fixtureDir(t))
			if err := l.Log(Entry{ID: "a", Phase: "intent", Path: "/a", RecoveryPath: "/r/a"}); err != nil {
				t.Fatal(err)
			}
			before := l.cache
			l.beforeAppend = func() {
				b, err := os.ReadFile(l.Path())
				if err != nil {
					t.Fatal(err)
				}
				if rewrite == "malformed" {
					b[0] = '!'
				} else {
					b = bytes.ReplaceAll(b, []byte("/a"), []byte("/b"))
				}
				f, err := os.OpenFile(l.Path(), os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteAt(b, 0); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			// Our append masks the prefix writer's mtime/ctime transition.
			// Its resulting stamp must never certify the old pending state.
			err := l.Log(Entry{ID: "a", Phase: "complete", Path: "/a", Error: strings.Repeat("x", 800*1024)})
			if err == nil || !strings.Contains(err.Error(), "history changed during append") {
				t.Fatalf("overlapping prefix rewrite accepted: %v", err)
			}
			if l.cache.id != before.id || l.cache.digest != before.digest || l.cache.pending["a"].Phase != "intent" {
				t.Fatal("failed append verification changed cached recovery state")
			}
			l.beforeAppend = nil
			if rewrite == "malformed" {
				assertJournalRefused(t, l)
				if _, err := New(l.Dir).Pending(); err == nil {
					t.Fatal("fresh recovery accepted malformed history")
				}
			}
		})
	}
}

func TestJournalRejectsChangesDuringReplay(t *testing.T) {
	l := New(fixtureDir(t))
	if err := os.WriteFile(l.Path(), journalRecord(t, Entry{ID: "a", Phase: "intent", Path: "/a", RecoveryPath: "/r/a"}), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := false
	l.readRecord = func() {
		if !changed {
			changed = true
			// An outside writer that ignores the advisory lock must still be
			// detected when it changes the file during a bounded replay.
			appendJournal(t, l.Path(), journalRecord(t, Entry{ID: "b", Phase: "intent", Path: "/b", RecoveryPath: "/r/b"}))
		}
	}
	if _, err := l.Pending(); err == nil {
		t.Fatal("cached a journal that changed during replay")
	}
	if l.cache.valid {
		t.Fatal("failed replay installed trusted state")
	}
	l.readRecord = nil
	pending, err := l.Pending()
	if err != nil || len(pending) != 2 {
		t.Fatalf("fresh validation after outside write: %+v, %v", pending, err)
	}
}

func TestJournalInvalidatesSameLengthRewriteWithRestoredMtime(t *testing.T) {
	l := New(fixtureDir(t))
	original := journalRecord(t, Entry{ID: "a", Phase: "intent", Path: "/a", RecoveryPath: "/r/a"})
	if err := os.WriteFile(l.Path(), original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	parsed := 0
	l.readRecord = func() { parsed++ }
	rewritten := bytes.ReplaceAll(original, []byte("/a"), []byte("/b"))
	if err := os.WriteFile(l.Path(), rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(l.Path(), fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	pending, err := l.Pending()
	if err != nil || len(pending) != 1 || pending[0].Path != "/b" || parsed != 1 {
		t.Fatalf("same-length rewrite: %+v, %d parsed, %v", pending, parsed, err)
	}
	rewritten[0] = '!'
	if err := os.WriteFile(l.Path(), rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(l.Path(), fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	assertJournalRefused(t, l)
}

func TestJournalInvalidatesSameSizeReplacement(t *testing.T) {
	l := New(fixtureDir(t))
	original := journalRecord(t, Entry{ID: "a", Phase: "intent", Path: "/a", RecoveryPath: "/r/a"})
	if err := os.WriteFile(l.Path(), original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); err != nil {
		t.Fatal(err)
	}
	parsed := 0
	l.readRecord = func() { parsed++ }
	replacement := filepath.Join(l.Dir, "replacement")
	rewritten := journalRecord(t, Entry{ID: "b", Phase: "intent", Path: "/b", RecoveryPath: "/r/b"})
	if len(rewritten) != len(original) {
		t.Fatal("replacement fixture has a different length")
	}
	if err := os.WriteFile(replacement, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, l.Path()); err != nil {
		t.Fatal(err)
	}
	pending, err := l.Pending()
	if err != nil || len(pending) != 1 || pending[0].ID != "b" || parsed != 1 {
		t.Fatalf("same-size replacement: %+v, %d parsed, %v", pending, parsed, err)
	}
	rewritten[0] = '!'
	if err := os.WriteFile(replacement, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, l.Path()); err != nil {
		t.Fatal(err)
	}
	assertJournalRefused(t, l)
}

func TestJournalRejectsObservedTruncationAndDisappearance(t *testing.T) {
	for _, mode := range []string{"record boundary", "incomplete record", "missing file", "missing directory"} {
		t.Run(mode, func(t *testing.T) {
			l := New(filepath.Join(fixtureDir(t), "SpaceHound"))
			for _, id := range []string{"a", "b"} {
				if err := l.Log(Entry{ID: id, Phase: "intent", Path: "/fixture", RecoveryPath: "/recovery"}); err != nil {
					t.Fatal(err)
				}
			}
			b, err := os.ReadFile(l.Path())
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "record boundary":
				err = os.Truncate(l.Path(), int64(bytes.IndexByte(b, '\n')+1))
			case "incomplete record":
				err = os.Truncate(l.Path(), int64(len(b)-1))
			case "missing file":
				err = os.Remove(l.Path())
			case "missing directory":
				err = os.RemoveAll(l.Dir)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := l.Pending(); err == nil {
				t.Error("Pending accepted lost journal history")
			}
			if err := l.Check(); err == nil {
				t.Error("Check accepted lost journal history")
			}
			if err := l.Log(Entry{Action: "trash", Path: "/fixture"}); err == nil {
				t.Error("Log accepted lost journal history")
			}
		})
	}
}

func TestConcurrentLoggersPreserveAllRecoveryPhases(t *testing.T) {
	dir := fixtureDir(t)
	const workers, operations = 4, 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			l := New(dir)
			for i := 0; i < operations; i++ {
				id := fmt.Sprintf("%d-%d", worker, i)
				for _, phase := range []string{"intent", "captured", "complete"} {
					if err := l.Log(Entry{ID: id, Phase: phase, Path: "/fixture", RecoveryPath: "/recovery"}); err != nil {
						errs <- err
						return
					}
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	fresh := New(dir)
	parsed := 0
	fresh.readRecord = func() { parsed++ }
	pending, err := fresh.Pending()
	if err != nil || len(pending) != 0 || parsed != workers*operations*3 {
		t.Fatalf("concurrent journal: %d pending, %d records, %v", len(pending), parsed, err)
	}
}

func BenchmarkJournalHistory(b *testing.B) {
	record := []byte("{\"operation_id\":\"finished\",\"phase\":\"complete\",\"path\":\"/fixture\"}\n")
	history := bytes.Repeat(record, 50000)
	for _, operation := range []string{"full replay", "cached check", "verified append"} {
		b.Run(operation, func(b *testing.B) {
			dir, err := filepath.EvalSymlinks(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			l := New(dir)
			if err := os.WriteFile(l.Path(), history, 0o600); err != nil {
				b.Fatal(err)
			}
			if err := l.Check(); err != nil {
				b.Fatal(err)
			}
			if operation != "cached check" {
				b.SetBytes(int64(len(history)))
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var err error
				switch operation {
				case "full replay":
					_, err = New(dir).Pending()
				case "cached check":
					err = l.Check()
				case "verified append":
					err = l.Log(Entry{ID: "new", Phase: "complete", Path: "/fixture"})
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func journalRecord(t *testing.T, e Entry) []byte {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func appendJournal(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertJournalRefused(t *testing.T, l *Logger) {
	t.Helper()
	b, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Pending(); err == nil {
		t.Error("Pending trusted changed invalid journal")
	}
	if err := l.Check(); err == nil {
		t.Error("Check trusted changed invalid journal")
	}
	if err := l.Log(Entry{Action: "trash", Path: "/fixture"}); err == nil {
		t.Error("Log trusted changed invalid journal")
	}
	assertContents(t, l.Path(), b)
}

func fixtureDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func assertContents(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s contents were changed", path)
	}
}

func aclListing(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("/bin/ls", "-lde", path).CombinedOutput()
	if err != nil {
		t.Fatalf("read fixture ACL: %v: %s", err, out)
	}
	return string(out)
}
