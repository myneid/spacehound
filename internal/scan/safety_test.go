package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"spacehound/internal/fsid"
	"spacehound/internal/macosfs"
	"spacehound/internal/model"
)

func TestProtectedRootAndAncestorCannotBeIncluded(t *testing.T) {
	base := scanTempDir(t)
	for _, name := range []string{".Trash", ".tRaSh", "Backups.backupdb", "copy.BACKUPBUNDLE", ".spotlight-v100", ".spacehound-hold-original", ".SpaceHound-Probe-testing", ".SPACEHOUND-PARTIAL-interrupted"} {
		root := filepath.Join(base, name)
		file := put(t, root, "nested/valuable.bin", 10, 1, now)
		for _, path := range []string{root, filepath.Dir(file)} {
			o := testOptions(base)
			o.Root, o.LargeThreshold = path, 1
			o.Includes = []string{root, path}
			rep, err := Run(context.Background(), o, nil)
			if err != nil {
				t.Fatal(err)
			}
			if rep.FilesSeen != 0 || len(rep.Items) != 0 || len(rep.Skipped) != 1 || rep.Skipped[0].Path != path {
				t.Errorf("protected root %q was scanned: %+v", path, rep)
			}
		}
	}
	for _, path := range []string{"/sYsTeM/Library", "/PRIVATE/VAR/DB/something", "/Library/APPLE/nested", "/Volumes/Disk/.TRASHES/501/nested/file"} {
		if ProtectedReason(path, base) == "" {
			t.Errorf("mandatory ancestor not protected: %s", path)
		}
	}
}

func TestRecoveryFilesNeverBecomeCandidates(t *testing.T) {
	base := scanTempDir(t)
	for _, name := range []string{".spacehound-hold-original", ".SpaceHound-Probe-testing", ".SPACEHOUND-PARTIAL-interrupted"} {
		put(t, base, name, 10, 1, now)
	}
	o := testOptions(base)
	o.LargeThreshold = 1
	o.Includes = []string{base}
	rep, err := Run(context.Background(), o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FilesSeen != 0 || len(rep.Items) != 0 || len(rep.Skipped) != 3 {
		t.Fatalf("recovery data became a cleanup candidate: %+v", rep)
	}
}

func TestExplicitOverridableRootAndRootExclude(t *testing.T) {
	base := scanTempDir(t)
	root := filepath.Join(base, "Library/CloudStorage/Provider")
	file := put(t, root, "valuable.bin", 10, 1, now)
	o := testOptions(base)
	o.Root, o.LargeThreshold = root, 1
	rep, err := Run(context.Background(), o, nil)
	if err != nil || index(rep)[file] == nil {
		t.Fatalf("explicit overridable root not scanned: report=%+v error=%v", rep, err)
	}
	o.Excludes = []string{root}
	rep, err = Run(context.Background(), o, nil)
	if err != nil || rep.FilesSeen != 0 || len(rep.Skipped) != 1 {
		t.Fatalf("root exclusion ignored: report=%+v error=%v", rep, err)
	}
}

func TestSensitivityOutsideHomeUsesWholeComponents(t *testing.T) {
	for _, path := range []string{
		"/Users/other/Library/Application Support/App/file.bin",
		"/Library/Containers/App/file.bin",
		"/Volumes/Archive/other/Library/Developer/CoreSimulator/Devices/file.bin",
		"/Users/other/.rustup/toolchains/file.bin",
		"/Volumes/Archive/other/go/pkg/mod/example/file.bin",
	} {
		if sensitivity(path, "") == "" {
			t.Errorf("sensitive path outside home was missed: %s", path)
		}
	}
	for _, path := range []string{"/Users/other/notLibrary/Application Support/file.bin", "/Users/other/Library/Containers-old/file.bin", "/Users/other/.rustup-old/file.bin"} {
		if got := sensitivity(path, ""); got != "" {
			t.Errorf("partial component unexpectedly matched: %s: %s", path, got)
		}
	}
}

func TestHashesRejectSymlinkAtAnyComponent(t *testing.T) {
	base := scanTempDir(t)
	file := put(t, base, "real/data.bin", 100, 1, now)
	linkFile := filepath.Join(base, "linked-file")
	if err := os.Symlink(file, linkFile); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(base, "linked-dir")
	if err := os.Symlink(filepath.Dir(file), linkDir); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{linkFile, filepath.Join(linkDir, "data.bin")} {
		if _, err := SampleHash(context.Background(), path, 100); err == nil {
			t.Errorf("sample followed symlink: %s", path)
		}
		if _, err := FullHash(context.Background(), path, nil); err == nil {
			t.Errorf("full hash followed symlink: %s", path)
		}
	}
	if _, err := SampleHash(context.Background(), file, 99); err == nil {
		t.Error("sample accepted stale size")
	}
}

func TestDescriptorWalkCannotBeRedirectedByAncestorSwap(t *testing.T) {
	base := scanTempDir(t)
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	put(t, root, "original.bin", 10, 1, now)
	put(t, outside, "secret.bin", 10, 2, now)
	dir, err := macosfs.OpenDir(root, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	id, _, err := fsid.FromOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-held"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	o := testOptions(base)
	o.LargeThreshold = 1
	s := &scanner{opts: &o, prog: &Progress{}, items: map[string]*model.Item{}}
	s.allowedDevices(id)
	s.walkDir(context.Background(), dir, root)
	if s.items[filepath.Join(root, "secret.bin")] != nil || len(s.items) != 1 || s.items[filepath.Join(root, "original.bin")] == nil {
		t.Fatalf("walk escaped held directory: %+v errors=%v", s.items, s.errs)
	}
	o.Root = root
	if _, err := Run(context.Background(), o, nil); err == nil {
		t.Error("root symlink accepted")
	}
}
