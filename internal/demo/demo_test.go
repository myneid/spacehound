package demo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spacehound/internal/model"
	"spacehound/internal/scan"
)

func TestDemoFixtureScan(t *testing.T) {
	t.Setenv("TMPDIR", "/private/var/folders/spacehound-demo-must-not-create")
	f, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Remove()
	if filepath.Dir(f.Dir) != fixtureRoot {
		t.Fatalf("fixture outside dedicated temporary root: %s", f.Dir)
	}
	if why := scan.ProtectedReason(f.Home, f.Home); why != "" {
		t.Fatalf("demo fixture protected: %s", why)
	}
	rep, err := scan.Run(context.Background(), f.Options(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Items) != 14 || len(rep.Errors) != 0 {
		t.Fatalf("demo scan incomplete: %d items, %v", len(rep.Items), rep.Errors)
	}
	cats := map[model.Category]int{}
	for _, it := range rep.Items {
		if strings.Contains(it.Path, ".Trash") || strings.Contains(it.Path, "Mobile Documents") || strings.Contains(it.Path, "Desktop-link") {
			t.Errorf("flagged excluded path %s", it.Path)
		}
		for _, c := range it.Categories {
			cats[c]++
		}
	}
	for _, c := range model.Categories {
		if cats[c] == 0 {
			t.Errorf("demo has no %s items", c)
		}
	}
	if len(rep.Duplicates) != 1 {
		t.Errorf("want 1 duplicate group, got %d", len(rep.Duplicates))
	}
	vols, err := f.Lister().List()
	if err != nil || len(vols) != 3 || !vols[0].Suitable || vols[1].Suitable || vols[2].Suitable {
		t.Errorf("demo volumes: %+v %v", vols, err)
	}
	dir := f.Dir
	t.Setenv("TMPDIR", "/private/tmp/spacehound-demo-missing-temp-root")
	if err := f.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("fixture not removed")
	}
}

func TestRemoveRefusesForeignDir(t *testing.T) {
	f := &Fixture{Dir: t.TempDir()}
	if err := f.Remove(); err == nil {
		t.Error("removed a non-demo directory")
	}
}
