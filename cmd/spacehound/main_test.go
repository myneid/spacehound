package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spacehound/internal/demo"
	"spacehound/internal/fsid"
	"spacehound/internal/macosfs"
	"spacehound/internal/model"
	"spacehound/internal/offload"
	"spacehound/internal/units"
)

func TestParseArgsFlags(t *testing.T) {
	c, err := parseArgs([]string{"--root", "/tmp/x", "--large", "500MB", "--dup-threshold", "10MiB",
		"--stale-days", "30", "--quick", "--exclude", "node_modules", "--exclude", "~/VMs", "--json"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	o := c.scanOptions(nil)
	if o.Root != "/tmp/x" || o.LargeThreshold != 500e6 || o.DupThreshold != 10*units.MiB ||
		o.StaleDays != 30 || o.Duplicates || !c.JSON {
		t.Errorf("unexpected options %+v", o)
	}
	if len(o.Excludes) != 2 || o.Excludes[0] != "node_modules" || o.Excludes[1] != "~/VMs" {
		t.Errorf("excludes = %v", o.Excludes)
	}
}

func TestParseArgsDefaults(t *testing.T) {
	c, err := parseArgs(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	o := c.scanOptions(nil)
	if o.LargeThreshold != units.GiB || o.DupThreshold != 50*units.MiB || o.StaleDays != 90 || !o.Duplicates {
		t.Errorf("defaults changed: %+v", o)
	}
}

func TestParseArgsRejects(t *testing.T) {
	for _, args := range [][]string{
		{"--large", "-1"},
		{"--large", "0"},
		{"--large", "lots"},
		{"--stale-days", "-3"},
		{"--exclude", ""},
		{"stray"},
		{"--demo", "--root", "/"},
		{"--demo", "--include", "~/Library/Mobile Documents"},
	} {
		if _, err := parseArgs(args, io.Discard); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
}

func TestDemoOptionsStayInFixture(t *testing.T) {
	fx := &demo.Fixture{Home: "/tmp/spacehound-demo-x/home"}
	c, err := parseArgs([]string{"--demo", "--stale-days", "7"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	o := c.scanOptions(fx)
	if o.Root != fx.Home || o.Home != fx.Home {
		t.Errorf("demo scan escapes fixture: root=%s home=%s", o.Root, o.Home)
	}
	if o.StaleDays != 7 || o.LargeThreshold != fx.Options().LargeThreshold {
		t.Errorf("demo overrides wrong: %+v", o)
	}
}

func TestRunDemoJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--demo", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var rep model.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Root, "spacehound-demo-") || len(rep.Items) == 0 || rep.Version != version {
		t.Errorf("unexpected demo report: root=%s items=%d", rep.Root, len(rep.Items))
	}
}

func TestRunErrorsCannotInjectTerminalControls(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--unknown\x1b[2J\n\u202e"}, &out, &errOut); code != 2 {
		t.Fatalf("want argument failure, got %d", code)
	}
	if strings.ContainsAny(errOut.String(), "\x1b\u202e") || !strings.Contains(errOut.String(), `\x1b`) {
		t.Fatalf("unsafe argument diagnostic: %q", errOut.String())
	}
	if got := shorten("\x1b[2J/path", 100); got != `\x1b[2J/path` {
		t.Fatalf("unsafe scan progress: %q", got)
	}
}

func TestDemoMutationWiringStaysInFixture(t *testing.T) {
	t.Setenv("TMPDIR", "/private/var/folders/spacehound-demo-must-not-create")
	fixture, err := demo.Build()
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Remove()
	env := newEnv(fixture.Home, fixture)
	path := filepath.Join(fixture.Home, "Movies", "Vacation 2019.mov")
	id, _, err := fsid.Of(path)
	if err != nil {
		t.Fatal(err)
	}
	trashed, err := env.Trash(path, id)
	if err != nil || !strings.HasPrefix(trashed.Dest, fixture.TrashDir+"/") {
		t.Fatalf("demo Trash escaped fixture: %+v %v", trashed, err)
	}
	vols, err := env.Volumes()
	if err != nil || len(vols) == 0 || !vols[0].Suitable {
		t.Fatal(vols, err)
	}
	path = filepath.Join(fixture.Home, "Downloads", "installer-disk.dmg")
	id, _, err = fsid.Of(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := env.Offloader.Offload(context.Background(), offload.Request{Source: path, Identity: id, Volume: vols[0]}, nil)
	if err != nil || !strings.HasPrefix(res.Dest, fixture.Volumes+"/") {
		t.Fatalf("demo copy escaped fixture: %+v %v", res, err)
	}
	if err := offload.RemoveSource(res); err != nil && !errors.Is(err, macosfs.ErrGuardedUnlinkUnsupported) {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.Dest); err != nil {
		t.Fatal("demo lost archive", err)
	}
	pending, err := env.Log.Pending()
	if err != nil || len(pending) != 0 {
		t.Fatal("unfinished demo operation", pending, err)
	}
}
