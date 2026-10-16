// Command spacehound is a safe macOS terminal UI for recovering disk space.
//
// Scanning is read-only. The interactive UI may move files to the Trash or
// offload them to an external drive, but only after a summary screen, a
// typed confirmation and a fresh identity check. --json never mutates.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"spacehound/internal/demo"
	"spacehound/internal/diskspace"
	"spacehound/internal/display"
	"spacehound/internal/model"
	"spacehound/internal/offload"
	"spacehound/internal/oplog"
	"spacehound/internal/scan"
	"spacehound/internal/term"
	"spacehound/internal/trash"
	"spacehound/internal/tui"
	"spacehound/internal/units"
	"spacehound/internal/volumes"
)

// version is overridden at release build time with -ldflags "-X main.version=...".
var version = "dev"

const usageText = `SpaceHound — find space to reclaim on macOS, safely.

Usage:
  spacehound [flags]

Scanning is read-only. Cleanup moves files to the Trash. Offloading copies
and verifies a complete file on an external drive; a separate confirmation
offers permanent removal of the original, moving it to Trash, or keeping
both. Every action requires typed confirmation. --json never changes files.

Flags:
  --root PATH            directory to scan (default: your home directory)
  --large SIZE           large-file threshold (default 1GiB; e.g. 500MB, 2G)
  --dup-threshold SIZE   minimum size for duplicate detection (default 50MiB)
  --stale-days N         age in days before a log counts as stale (default 90)
  --no-duplicates        skip duplicate detection (alias: --quick)
  --exclude PATTERN      skip a path (contains "/" or starts with "~") or a
                         base-name glob such as "node_modules"; repeatable
  --include PATH         scan a path that is skipped by default (only
                         overridable defaults; see --show-defaults); repeatable
  --cross-devices        descend into other mounted filesystems
  --json                 print a JSON report to stdout and exit (no UI)
  --demo                 scan a generated temporary fixture instead of real data
  --no-color             disable colors in the UI (NO_COLOR is also honoured)
  --show-defaults        print the built-in skip rules and exit
  --version              print the version and exit
  -h, --help             show this help
`

// config is the parsed command line.
type config struct {
	Root         string
	Large        int64
	DupThreshold int64
	StaleDays    int
	NoDuplicates bool
	Excludes     []string
	Includes     []string
	CrossDevices bool
	JSON         bool
	Demo         bool
	NoColor      bool
	ShowDefaults bool
	Version      bool
	Help         bool
	// set records which flags were given explicitly.
	set map[string]bool
}

// multiFlag collects repeatable string flags.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("must not be empty")
	}
	*m = append(*m, s)
	return nil
}

// sizeFlag parses human sizes such as "1GiB".
type sizeFlag struct{ v *int64 }

func (s sizeFlag) String() string {
	if s.v == nil {
		return ""
	}
	return units.FormatSize(*s.v)
}
func (s sizeFlag) Set(v string) error {
	n, err := units.ParseSize(v)
	if err != nil {
		return err
	}
	if n <= 0 {
		return errors.New("must be positive")
	}
	*s.v = n
	return nil
}

// parseArgs parses args (without the program name) on top of the
// documented scan defaults.
func parseArgs(args []string, stderr io.Writer) (*config, error) {
	d := scan.DefaultOptions()
	c := &config{Large: d.LargeThreshold, DupThreshold: d.DupThreshold, StaleDays: d.StaleDays, set: map[string]bool{}}
	fs := flag.NewFlagSet("spacehound", flag.ContinueOnError)
	// The caller prints the returned error through the terminal sanitizer.
	// flag's own diagnostics can contain untrusted argument text.
	fs.SetOutput(io.Discard)
	fs.Usage = func() { fmt.Fprint(stderr, usageText) }
	fs.StringVar(&c.Root, "root", "", "")
	fs.Var(sizeFlag{&c.Large}, "large", "")
	fs.Var(sizeFlag{&c.DupThreshold}, "dup-threshold", "")
	fs.IntVar(&c.StaleDays, "stale-days", c.StaleDays, "")
	fs.BoolVar(&c.NoDuplicates, "no-duplicates", false, "")
	fs.BoolVar(&c.NoDuplicates, "quick", false, "")
	fs.Var((*multiFlag)(&c.Excludes), "exclude", "")
	fs.Var((*multiFlag)(&c.Includes), "include", "")
	fs.BoolVar(&c.CrossDevices, "cross-devices", false, "")
	fs.BoolVar(&c.JSON, "json", false, "")
	fs.BoolVar(&c.Demo, "demo", false, "")
	fs.BoolVar(&c.NoColor, "no-color", false, "")
	fs.BoolVar(&c.ShowDefaults, "show-defaults", false, "")
	fs.BoolVar(&c.Version, "version", false, "")
	fs.BoolVar(&c.Help, "help", false, "")
	fs.BoolVar(&c.Help, "h", false, "")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q (use --root PATH to choose what to scan)", fs.Arg(0))
	}
	fs.Visit(func(f *flag.Flag) { c.set[f.Name] = true })
	if c.StaleDays < 0 {
		return nil, errors.New("--stale-days must not be negative")
	}
	if c.Demo && (c.set["root"] || len(c.Includes) > 0 || c.CrossDevices) {
		return nil, errors.New("--demo scans only its generated fixture; it cannot be combined with --root, --include or --cross-devices")
	}
	return c, nil
}

// scanOptions builds scan options. In demo mode it starts from the
// fixture's scaled-down thresholds and only applies explicit overrides.
func (c *config) scanOptions(fx *demo.Fixture) scan.Options {
	var o scan.Options
	if fx != nil {
		o = fx.Options()
	} else {
		o = scan.DefaultOptions()
		if c.Root != "" {
			o.Root = c.Root
		}
		o.Includes = append(o.Includes, c.Includes...)
		o.CrossDevices = c.CrossDevices
	}
	if fx == nil || c.set["large"] {
		o.LargeThreshold = c.Large
	}
	if fx == nil || c.set["dup-threshold"] {
		o.DupThreshold = c.DupThreshold
	}
	if fx == nil || c.set["stale-days"] {
		o.StaleDays = c.StaleDays
	}
	if c.NoDuplicates {
		o.Duplicates = false
	}
	o.Excludes = append(o.Excludes, c.Excludes...)
	return o
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	c, err := parseArgs(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "spacehound:", display.Text(err.Error()))
		return 2
	}
	switch {
	case c.Help:
		fmt.Fprint(stdout, usageText)
		return 0
	case c.Version:
		fmt.Fprintln(stdout, "spacehound", display.Text(version))
		return 0
	case c.ShowDefaults:
		fmt.Fprintln(stdout, "Directories SpaceHound does not scan by default:")
		for _, r := range scan.DefaultSkipRules {
			fmt.Fprintln(stdout, " ", r.String())
		}
		return 0
	}
	if !c.JSON && (!term.IsTerminal(term.Stdin()) || !term.IsTerminal(term.Stdout())) {
		fmt.Fprintln(stderr, "spacehound: the interactive UI needs a terminal; use --json for scripting")
		return 2
	}

	var fx *demo.Fixture
	if c.Demo {
		if fx, err = demo.Build(); err != nil {
			fmt.Fprintln(stderr, "spacehound: building demo fixture:", display.Text(err.Error()))
			return 1
		}
		defer fx.Remove()
	}
	opts := c.scanOptions(fx)

	rep, err := runScan(opts, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "spacehound:", display.Text(err.Error()))
		return 1
	}
	rep.Version = version
	freePath := rep.Home
	if fx != nil {
		freePath = fx.Dir
		rep.Notes = append(rep.Notes, "Demo mode: results come from a generated temporary fixture, not your files.")
	}
	if info, err := diskspace.Statfs(freePath); err == nil {
		rep.FreeBytes, rep.TotalBytes = info.Free, info.Total
	}

	if c.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(stderr, "spacehound:", display.Text(err.Error()))
			return 1
		}
		return 0
	}

	env := newEnv(rep.Home, fx)
	pending, pendingErr := env.Log.Pending()
	if pendingErr != nil {
		fmt.Fprintln(stderr, "spacehound: operation journal requires inspection:", display.Text(pendingErr.Error()))
		return 1
	}
	for _, entry := range pending {
		rep.Notes = append(rep.Notes, fmt.Sprintf("Unfinished %s for %s; inspect recovery location %s and destination %s (a path may no longer exist if the action completed before the journal was updated)", entry.Action, entry.Path, entry.RecoveryPath, entry.Dest))
		fmt.Fprintln(stderr, display.Text(rep.Notes[len(rep.Notes)-1]))
	}
	m := tui.New(rep, env)
	if len(pending) > 0 {
		m.Update(tui.RecoveryMsg{Entries: pending})
	}
	m.Color = !c.NoColor && os.Getenv("NO_COLOR") == ""
	if fx != nil {
		m.Title = "DEMO"
	}
	if err := tui.Run(m); err != nil {
		fmt.Fprintln(stderr, "spacehound:", display.Text(err.Error()))
		return 1
	}
	return 0
}

// runScan scans with a progress line on a terminal stderr. Ctrl-C or
// SIGTERM cancels cleanly and yields a partial report.
func runScan(opts scan.Options, stderr io.Writer) (*model.Report, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	prog := &scan.Progress{}
	done, cleared := make(chan struct{}), make(chan struct{})
	showProgress := stderr == io.Writer(os.Stderr) && term.IsTerminal(int(os.Stderr.Fd()))
	if showProgress {
		go func() {
			defer close(cleared)
			t := time.NewTicker(150 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-done:
					fmt.Fprint(stderr, "\r\x1b[K")
					return
				case <-t.C:
					phase, cur := prog.Snapshot()
					fmt.Fprintf(stderr, "\r\x1b[KScanning (%s) %d files, %s · %s  [Ctrl-C to stop]",
						display.Text(phase), prog.Files.Load(), units.FormatSize(prog.Bytes.Load()), shorten(cur, 50))
				}
			}
		}()
	}
	rep, err := scan.Run(ctx, opts, prog)
	close(done)
	if showProgress {
		<-cleared
	}
	return rep, err
}

func shorten(s string, n int) string {
	s = display.Text(s)
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n+1:])
}

// newEnv wires the UI's side effects. In demo mode every operation is
// confined to the fixture: its own Trash folder, fake /Volumes and log
// directory, so --demo never touches real files.
func newEnv(home string, fx *demo.Fixture) tui.Env {
	if fx != nil {
		journal := oplog.New(fx.LogDir)
		tr := &trash.Trasher{Home: fx.Home, Journal: journal, DirFor: func(string, uint64) (string, error) { return fx.TrashDir, nil }}
		off := offload.New(fx.Home)
		off.Host, off.Statfs, off.AllowSameDevice, off.Journal = "demo-mac", fx.Statfs, true, journal
		return tui.Env{Trash: tr.Trash, Volumes: fx.Lister().List, Offloader: off, FreeSpace: func() (diskspace.Info, error) { return diskspace.Statfs(fx.Dir) }, Log: journal, Now: time.Now}
	}
	journal := oplog.New(oplog.DefaultDir(home))
	tr := trash.New(home)
	tr.Journal = journal
	off := offload.New(home)
	off.Journal = journal
	return tui.Env{Trash: tr.Trash, Volumes: volumes.NewLister(home).List, Offloader: off, FreeSpace: func() (diskspace.Info, error) { return diskspace.Statfs(home) }, Log: journal, Now: time.Now}
}
