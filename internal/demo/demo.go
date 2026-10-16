// Package demo builds a deterministic fixture tree under /private/tmp
// directory so SpaceHound's scanner and UI can be exercised without looking
// at real user data.
package demo

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"spacehound/internal/diskspace"
	"spacehound/internal/scan"
	"spacehound/internal/units"
	"spacehound/internal/volumes"
)

// Now is the fixed reference time used for demo ages.
var Now = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// macOS's usual TMPDIR is below /private/var/folders, a protected scan root.
const fixtureRoot = "/private/tmp"

// Fixture is a generated demo environment. Everything lives under Dir.
type Fixture struct {
	Dir      string // temp directory owning everything below
	Home     string // fake home directory (scan root)
	Volumes  string // fake /Volumes
	TrashDir string
	LogDir   string
}

type file struct {
	rel     string
	size    int64
	seed    byte
	ageDays int
}

var files = []file{
	{"Movies/Vacation 2019.mov", 6 * units.MiB, 1, 400},
	{"Movies/Screen Recording 2025-11-02.mov", 3 * units.MiB, 2, 200},
	{"Downloads/installer-disk.dmg", 4 * units.MiB, 3, 120},
	{"Downloads/ubuntu-24.04.iso", 3 * units.MiB, 4, 300},
	{"Documents/Archive/ubuntu-24.04.iso", 3 * units.MiB, 4, 310},
	{"Documents/Projects/render-final.mp4", 2*units.MiB + 512*units.KiB, 5, 10},
	{"Library/Application Support/DemoVM/Disk.vmdk", 5 * units.MiB, 6, 50},
	{"Library/Application Support/DemoApp/library.sqlite", 3 * units.MiB, 7, 400},
	{"Library/Logs/DemoApp/demo-2025-01.log", 1536 * units.KiB, 8, 500},
	{"Library/Logs/DemoApp/current.log", 1536 * units.KiB, 9, 1},
	{"code/webapp/npm-debug.log", 1200 * units.KiB, 10, 200},
	{"Library/Caches/com.example.browser/Cache/data_3", 2 * units.MiB, 11, 60},
	{"Library/Caches/com.example.browser/Cache/index.db", 2 * units.MiB, 12, 60},
	{"Library/Caches/com.apple.demo/blob", 1536 * units.KiB, 13, 90},
	{".cache/huggingface/hub/models--demo/model.safetensors", 2 * units.MiB, 14, 120},
	{".Trash/already-trashed.zip", 4 * units.MiB, 15, 30},
	{"Library/Mobile Documents/com~apple~CloudDocs/cloud.bin", 4 * units.MiB, 16, 30},
	{"Documents/notes.txt", 2 * units.KiB, 17, 3},
}

// Build creates a new fixture under /private/tmp, independently of TMPDIR.
func Build() (*Fixture, error) {
	dir, err := os.MkdirTemp(fixtureRoot, "spacehound-demo-")
	if err != nil {
		return nil, err
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return nil, err
	}
	f := &Fixture{
		Dir:     dir,
		Home:    filepath.Join(dir, "home"),
		Volumes: filepath.Join(dir, "Volumes"),
	}
	f.TrashDir = filepath.Join(f.Home, ".Trash")
	f.LogDir = filepath.Join(dir, "logs")
	for _, fl := range files {
		if err := write(filepath.Join(f.Home, fl.rel), fl.size, fl.seed, Now.AddDate(0, 0, -fl.ageDays)); err != nil {
			f.Remove()
			return nil, err
		}
	}
	// A symlink to a large file must not be followed or double counted.
	if err := os.Symlink(filepath.Join(f.Home, files[0].rel), filepath.Join(f.Home, "Desktop-link.mov")); err != nil {
		f.Remove()
		return nil, err
	}
	for _, d := range []string{"Demo Drive", "Time Machine", "Read Only"} {
		if err := os.MkdirAll(filepath.Join(f.Volumes, d), 0o755); err != nil {
			f.Remove()
			return nil, err
		}
	}
	os.Mkdir(filepath.Join(f.Volumes, "Time Machine", "Backups.backupdb"), 0o755)
	return f, nil
}

func write(p string, size int64, seed byte, mt time.Time) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = seed + byte(i%251)
	}
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		return err
	}
	return os.Chtimes(p, mt, mt)
}

// Remove deletes the fixture directory. It refuses to act on anything that
// does not look like a directory Build created.
func (f *Fixture) Remove() error {
	if f == nil || f.Dir == "" {
		return nil
	}
	if filepath.Dir(f.Dir) != fixtureRoot || len(filepath.Base(f.Dir)) < len("spacehound-demo-") ||
		filepath.Base(f.Dir)[:len("spacehound-demo-")] != "spacehound-demo-" {
		return fmt.Errorf("refusing to remove %s: not a demo fixture", f.Dir)
	}
	return os.RemoveAll(f.Dir)
}

// Options returns scan options scaled to the fixture's small files.
func (f *Fixture) Options() scan.Options {
	return scan.Options{
		Root:           f.Home,
		Home:           f.Home,
		LargeThreshold: 2 * units.MiB,
		Duplicates:     true,
		DupThreshold:   units.MiB,
		StaleDays:      90,
		LogMinSize:     units.MiB,
		CacheDays:      30,
		CacheMinSize:   units.MiB,
		Now:            Now,
	}
}

// Statfs fakes capacity for the demo volumes.
func (f *Fixture) Statfs(p string) (diskspace.Info, error) {
	info := diskspace.Info{Path: p, MountPoint: p, FSType: "apfs", Flags: diskspace.FlagLocal,
		Free: 400 * units.GiB, Total: 1000 * units.GiB}
	if filepath.Base(p) == "Read Only" {
		info.Flags |= diskspace.FlagReadOnly
	}
	if filepath.Dir(p) != f.Volumes {
		return diskspace.Statfs(p)
	}
	return info, nil
}

// Lister returns a volume lister over the fake /Volumes.
func (f *Fixture) Lister() *volumes.Lister {
	return &volumes.Lister{Root: f.Volumes, Statfs: f.Statfs, Writable: func(string) bool { return true }}
}
