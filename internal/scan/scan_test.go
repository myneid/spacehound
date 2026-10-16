package scan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"spacehound/internal/fsid"
	"spacehound/internal/model"
	"spacehound/internal/units"
)

var now = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func put(t *testing.T, root, rel string, size int64, seed byte, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = seed + byte(i%251)
	}
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

func testOptions(root string) Options {
	return Options{
		Root:           root,
		Home:           root,
		LargeThreshold: 4 * units.MiB,
		Duplicates:     true,
		DupThreshold:   units.MiB,
		StaleDays:      90,
		LogMinSize:     units.MiB,
		CacheDays:      30,
		CacheMinSize:   units.MiB,
		Now:            now,
	}
}

func scanTempDir(t *testing.T) string {
	t.Helper()
	// Mandatory exclusions include macOS's per-user /private/var/folders
	// tree. Generate fixtures outside it so tests exercise real policy.
	dir, err := os.MkdirTemp("/private/tmp", "spacehound-scan-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

type fixture struct {
	root string
	p    map[string]string
}

func buildFixture(t *testing.T) fixture {
	root := scanTempDir(t)
	// macOS temp dirs live behind the /var -> /private/var symlink.
	root, _ = filepath.EvalSymlinks(root)
	old := now.AddDate(0, 0, -200)
	fresh := now.Add(-time.Hour)
	f := fixture{root: root, p: map[string]string{}}
	add := func(key, rel string, size int64, seed byte, mt time.Time) {
		f.p[key] = put(t, root, rel, size, seed, mt)
	}
	add("bigmovie", "Movies/holiday.mov", 5*units.MiB, 1, old)
	add("appdata", "Library/Application Support/SomeApp/data.bin", 5*units.MiB, 2, old)
	add("oldlog", "Library/Logs/SomeApp/old.log", 2*units.MiB, 3, old)
	add("newlog", "Library/Logs/SomeApp/new.log", 2*units.MiB, 4, fresh)
	add("projlog", "code/proj/build.log", 2*units.MiB, 5, old)
	add("leveldb", "Library/Application Support/Chrome/IndexedDB/000003.log", 2*units.MiB, 6, old)
	add("cache", "Library/Caches/com.example.app/blob.bin", 3*units.MiB, 7, old)
	add("applecache", "Library/Caches/com.apple.Something/blob.bin", 3*units.MiB, 8, old)
	add("cachedb", "Library/Caches/com.example.app/store.sqlite", 3*units.MiB, 9, old)
	add("freshcache", "Library/Caches/com.example.app/fresh.bin", 3*units.MiB, 10, fresh)
	add("model", ".cache/huggingface/hub/models/m.bin", 3*units.MiB, 11, old)
	add("dupA", "Documents/disk.iso", 2*units.MiB, 12, old)
	add("dupB", "Downloads/disk copy.iso", 2*units.MiB, 12, fresh)
	add("hard1", "Documents/hard1.bin", 2*units.MiB, 13, old)
	add("same1", "Documents/same1.bin", 2*units.MiB, 14, old)
	add("trash", ".Trash/huge.bin", 5*units.MiB, 15, old)
	add("icloud", "Library/Mobile Documents/com~apple~CloudDocs/big.bin", 5*units.MiB, 16, old)
	add("small", "Documents/small.txt", 10, 17, old)

	// same2: same size as same1, differs only in the middle.
	b, _ := os.ReadFile(f.p["same1"])
	b[len(b)/2] ^= 0xff
	f.p["same2"] = filepath.Join(root, "Documents/same2.bin")
	os.WriteFile(f.p["same2"], b, 0o644)
	os.Chtimes(f.p["same2"], old, old)

	f.p["hard2"] = filepath.Join(root, "Documents/hard2.bin")
	if err := os.Link(f.p["hard1"], f.p["hard2"]); err != nil {
		t.Fatal(err)
	}
	f.p["link"] = filepath.Join(root, "Documents/movie-link.mov")
	if err := os.Symlink(f.p["bigmovie"], f.p["link"]); err != nil {
		t.Fatal(err)
	}
	return f
}

func index(rep *model.Report) map[string]*model.Item {
	m := map[string]*model.Item{}
	for _, it := range rep.Items {
		m[it.Path] = it
	}
	return m
}

func TestScanFixture(t *testing.T) {
	f := buildFixture(t)
	rep, err := Run(context.Background(), testOptions(f.root), nil)
	if err != nil {
		t.Fatal(err)
	}
	items := index(rep)

	expect := []struct {
		key  string
		cat  model.Category
		risk model.Risk
	}{
		{"bigmovie", model.CatLarge, model.RiskReview},
		{"appdata", model.CatLarge, model.RiskDangerous},
		{"oldlog", model.CatStaleLog, model.RiskLow},
		{"projlog", model.CatStaleLog, model.RiskReview},
		{"leveldb", model.CatStaleLog, model.RiskDangerous},
		{"cache", model.CatCache, model.RiskLow},
		{"applecache", model.CatCache, model.RiskReview},
		{"model", model.CatCache, model.RiskReview},
	}
	for _, e := range expect {
		it := items[f.p[e.key]]
		if it == nil {
			t.Errorf("%s (%s) not flagged", e.key, f.p[e.key])
			continue
		}
		if !it.Has(e.cat) || it.Risk != e.risk {
			t.Errorf("%s: got cats %v risk %s, want %s/%s; reasons %q", e.key, it.Categories, it.Risk, e.cat, e.risk, it.Reasons)
		}
		if len(it.Reasons) == 0 {
			t.Errorf("%s has no reasons", e.key)
		}
	}

	for _, key := range []string{"newlog", "cachedb", "freshcache", "trash", "icloud", "small", "link", "hard1", "hard2", "same1", "same2"} {
		if it := items[f.p[key]]; it != nil {
			t.Errorf("%s should not be flagged, got %v %q", key, it.Categories, it.Reasons)
		}
	}

	// Exactly one duplicate group: disk.iso (older, kept) + copy.
	if len(rep.Duplicates) != 1 {
		t.Fatalf("want 1 duplicate group, got %+v", rep.Duplicates)
	}
	g := rep.Duplicates[0]
	if g.Keep != f.p["dupA"] || len(g.Copies) != 1 || g.Copies[0] != f.p["dupB"] {
		t.Errorf("unexpected group %+v", g)
	}
	if g.Recoverable != 2*units.MiB || rep.Summary.DuplicateRecoverable != 2*units.MiB {
		t.Errorf("recoverable = %d / %d", g.Recoverable, rep.Summary.DuplicateRecoverable)
	}
	if it := items[f.p["dupB"]]; it == nil || !it.Has(model.CatDuplicate) || it.DupGroup != 1 {
		t.Errorf("dupB not flagged as duplicate: %+v", it)
	}
	if items[f.p["dupA"]] != nil {
		t.Errorf("kept copy should not be listed as removable")
	}

	// Summary counts each file once.
	var sum int64
	for _, it := range rep.Items {
		sum += it.Size
	}
	if rep.Summary.Total.Size != sum || rep.Summary.Total.Count != len(rep.Items) {
		t.Errorf("summary %+v does not match items (%d bytes)", rep.Summary.Total, sum)
	}

	skipped := map[string]bool{}
	for _, s := range rep.Skipped {
		skipped[s.Path] = true
	}
	for _, rel := range []string{".Trash", "Library/Mobile Documents"} {
		if !skipped[filepath.Join(f.root, rel)] {
			t.Errorf("%s not reported as skipped: %+v", rel, rep.Skipped)
		}
	}
}

func TestScanIncludeExclude(t *testing.T) {
	f := buildFixture(t)
	o := testOptions(f.root)
	o.Includes = []string{filepath.Join(f.root, "Library/Mobile Documents")}
	o.Excludes = []string{"*.mov", filepath.Join(f.root, "code")}
	o.Duplicates = false
	rep, err := Run(context.Background(), o, nil)
	if err != nil {
		t.Fatal(err)
	}
	items := index(rep)
	if items[f.p["icloud"]] == nil {
		t.Errorf("--include did not re-enable iCloud Drive scanning")
	}
	if items[f.p["bigmovie"]] != nil || items[f.p["projlog"]] != nil {
		t.Errorf("--exclude not honoured")
	}
	if len(rep.Duplicates) != 0 || items[f.p["dupB"]] != nil {
		t.Errorf("duplicates should be disabled")
	}

	// The Trash can never be included.
	o.Includes = []string{filepath.Join(f.root, ".Trash")}
	rep, _ = Run(context.Background(), o, nil)
	if index(rep)[f.p["trash"]] != nil {
		t.Errorf("Trash must never be scanned")
	}
}

func TestScanCancelled(t *testing.T) {
	f := buildFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := Run(ctx, testOptions(f.root), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Cancelled || len(rep.Items) != 0 {
		t.Errorf("cancelled scan: cancelled=%v items=%d", rep.Cancelled, len(rep.Items))
	}
}

func TestDuplicateHashingStages(t *testing.T) {
	root := scanTempDir(t)
	old := now.AddDate(-1, 0, 0)
	o := testOptions(root)
	o.Normalize()
	s := &scanner{opts: &o, prog: &Progress{}, items: map[string]*model.Item{}}

	mk := func(name string, seed byte, size int64) fileInfo {
		p := put(t, root, name, size, seed, old)
		fi, _ := os.Lstat(p)
		id, _ := fsidOf(fi)
		return fileInfo{Path: p, ModTime: fi.ModTime(), ID: id}
	}
	a := mk("a", 1, 3*units.MiB)
	b := mk("b", 2, 3*units.MiB) // same size, different start: sample differs
	c := mk("c", 3, 2*units.MiB)
	d := mk("d", 3, 2*units.MiB) // identical to c
	e := mk("e", 9, units.MiB)   // unique size: never hashed
	alias := filepath.Join(root, "c-alias")
	os.Link(c.Path, alias)
	fi, _ := os.Lstat(alias)
	aid, _ := fsidOf(fi)
	cAlias := fileInfo{Path: alias, ModTime: fi.ModTime(), ID: aid}
	// Creating the hard link changes ctime and link count on both aliases.
	c.ID = aid

	var samples, fulls atomic.Int32
	hs := hashers{
		sample: func(ctx context.Context, p string, size int64) (string, error) {
			samples.Add(1)
			return SampleHash(ctx, p, size)
		},
		full: func(ctx context.Context, p string, prog *Progress) (string, error) {
			fulls.Add(1)
			return FullHash(ctx, p, prog)
		},
	}
	groups := s.groupDuplicates(context.Background(), []fileInfo{a, b, c, cAlias, d, e}, hs)
	if len(groups) != 1 || len(groups[0].files) != 2 {
		t.Fatalf("want one group of 2, got %+v", groups)
	}
	got := []string{groups[0].files[0].Path, groups[0].files[1].Path}
	if !(strings.HasSuffix(got[0], "/c") || strings.HasSuffix(got[0], "c-alias")) || !strings.HasSuffix(got[1], "/d") {
		t.Errorf("unexpected group members %v", got)
	}
	if samples.Load() != 4 {
		t.Errorf("sample hashes = %d, want 4 (a,b,c,d; alias and unique size skipped)", samples.Load())
	}
	if fulls.Load() != 2 {
		t.Errorf("full hashes = %d, want 2 (only c,d survive sampling)", fulls.Load())
	}
}

func TestSkipRules(t *testing.T) {
	o := Options{Root: "/Users/me", Home: "/Users/me"}
	cases := map[string]bool{
		"/Users/me/.Trash":                                true,
		"/Users/me/Library/Mobile Documents":              true,
		"/Users/me/Library/CloudStorage":                  true,
		"/Users/me/Pictures/Photos Library.photoslibrary": true,
		"/Users/me/Documents":                             false,
		"/System":                                         true,
		"/Volumes":                                        true,
		"/Volumes/Backup/Backups.backupdb":                true,
		"/Users/me/Library/Caches":                        false,
	}
	for p, want := range cases {
		if got := o.skipReason(p, true) != ""; got != want {
			t.Errorf("skip(%s) = %v, want %v", p, got, want)
		}
	}
	// Explicit root inside an overridable skip is scanned.
	o.Root = "/Users/me/Library/CloudStorage/Dropbox"
	if o.skipReason("/Users/me/Library/CloudStorage", true) != "" {
		t.Errorf("explicit root should lift overridable rule")
	}
}

func TestSensitivity(t *testing.T) {
	home := "/Users/me"
	for p, want := range map[string]bool{
		"/Users/me/Library/Caches/x/y.sqlite":                 true,
		"/Users/me/Library/Application Support/Foo/big.bin":   true,
		"/Users/me/VMs/win.vmdk":                              true,
		"/Users/me/Parallels/Win.pvm/disk.hds":                true,
		"/Users/me/.bitcoin/blocks/blk00001.dat":              true,
		"/Users/me/Library/Developer/CoreSimulator/Devices/x": true,
		"/Users/me/.rustup/toolchains/x/lib.a":                true,
		"/Users/me/x/leveldb/000012.log":                      true,
		"/Users/me/Movies/holiday.mov":                        false,
		"/Users/me/Library/Caches/com.spotify.client/Data/a":  false,
		"/Users/me/Library/Logs/x/old.log":                    false,
	} {
		if got := sensitivity(p, homeRel(p, home)) != ""; got != want {
			t.Errorf("sensitivity(%s) = %v, want %v", p, got, want)
		}
	}
}

func TestSparseAnnotation(t *testing.T) {
	root := scanTempDir(t)
	root, _ = filepath.EvalSymlinks(root)
	p := filepath.Join(root, "sparse.img.bin")
	fh, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	fh.Truncate(8 * units.MiB)
	fh.Close()
	o := testOptions(root)
	rep, err := Run(context.Background(), o, nil)
	if err != nil {
		t.Fatal(err)
	}
	it := index(rep)[p]
	if it == nil {
		t.Fatal("sparse large file not flagged")
	}
	if it.Allocated >= it.Size {
		t.Skip("filesystem did not create a sparse file")
	}
	found := false
	for _, r := range it.Reasons {
		found = found || strings.Contains(r, "Sparse")
	}
	if !found || it.LikelySavings() >= it.Size {
		t.Errorf("sparse file should not promise full savings: %q savings=%d", it.Reasons, it.LikelySavings())
	}
}

func fsidOf(fi os.FileInfo) (fsid.Identity, bool) { return fsid.FromFileInfo(fi) }
