// Package scan walks a directory tree read-only and classifies files that
// may be worth removing or offloading. It never modifies anything.
package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"spacehound/internal/fsid"
	"spacehound/internal/macosfs"
	"spacehound/internal/model"
)

// sfDataless marks APFS cloud placeholders whose contents are not local;
// reading them would trigger a download.
const sfDataless = 0x40000000

// Progress is updated concurrently by a running scan and may be read at any
// time from another goroutine.
type Progress struct {
	Files     atomic.Int64
	Dirs      atomic.Int64
	Bytes     atomic.Int64
	Errors    atomic.Int64
	HashTotal atomic.Int64
	HashDone  atomic.Int64
	mu        sync.Mutex
	phase     string
	current   string
}

func (p *Progress) set(phase, current string) {
	p.mu.Lock()
	p.phase, p.current = phase, current
	p.mu.Unlock()
}

// Snapshot returns the current phase and path being processed.
func (p *Progress) Snapshot() (phase, current string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.phase, p.current
}

// Run performs a scan. Cancelling ctx stops promptly and returns a partial
// report with Cancelled set; the error is nil in that case.
func Run(ctx context.Context, opts Options, prog *Progress) (*model.Report, error) {
	if err := opts.Normalize(); err != nil {
		return nil, err
	}
	if prog == nil {
		prog = &Progress{}
	}
	s := &scanner{opts: &opts, prog: prog, items: map[string]*model.Item{}}
	rep := &model.Report{
		Tool:      "spacehound",
		Root:      opts.Root,
		Home:      opts.Home,
		StartedAt: time.Now(),
		Options:   opts,
	}
	if err := s.walk(ctx); err != nil {
		return nil, err
	}
	var dups []model.DuplicateGroup
	if opts.Duplicates && ctx.Err() == nil {
		dups = s.findDuplicates(ctx)
	}
	rep.Cancelled = ctx.Err() != nil
	if rep.Cancelled {
		rep.Notes = append(rep.Notes, "Scan was cancelled; results are partial.")
		if opts.Duplicates {
			rep.Notes = append(rep.Notes, "Duplicate detection did not complete.")
		}
	}
	for _, it := range s.items {
		rep.Items = append(rep.Items, it)
	}
	model.SortItems(rep.Items)
	rep.Duplicates = dups
	rep.Skipped = s.skipped
	rep.Errors = s.errs
	rep.ErrorCount = int(prog.Errors.Load())
	rep.FilesSeen = prog.Files.Load()
	rep.DirsSeen = prog.Dirs.Load()
	rep.BytesSeen = prog.Bytes.Load()
	rep.Summary = model.Summarize(rep.Items, dups)
	rep.FinishedAt = time.Now()
	prog.set("done", "")
	return rep, nil
}

type scanner struct {
	opts      *Options
	prog      *Progress
	devs      map[uint64]bool
	items     map[string]*model.Item
	dupCands  []fileInfo
	skipped   []model.Skip
	errs      []string
	skipCount int
}

func (s *scanner) skip(path, reason string) {
	s.skipCount++
	if len(s.skipped) < s.opts.MaxRecorded {
		s.skipped = append(s.skipped, model.Skip{Path: path, Reason: reason})
	}
}

func (s *scanner) fail(path string, err error) {
	s.prog.Errors.Add(1)
	if len(s.errs) < s.opts.MaxRecorded {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		s.errs = append(s.errs, fmt.Sprintf("%s: %v", path, err))
	}
}

// allowedDevices returns the filesystems the walk may visit. On macOS the
// home directory lives on the Data volume, which is firmlinked into "/";
// scanning "/" therefore also admits the Data volume.
func (s *scanner) allowedDevices(id fsid.Identity) {
	s.devs = map[uint64]bool{id.Dev: true}
	if s.opts.Root == "/" {
		if d, _, err := fsid.Of("/System/Volumes/Data"); err == nil {
			s.devs[d.Dev] = true
		}
	}
}

func (s *scanner) walk(ctx context.Context) error {
	o := s.opts
	s.prog.set("scanning", o.Root)
	if reason := o.skipReason(o.Root, true); reason != "" {
		s.skip(o.Root, reason)
		return nil
	}
	root, err := macosfs.OpenDir(o.Root, false, 0)
	if err != nil {
		return fmt.Errorf("scan root: %w", err)
	}
	defer root.Close()
	id, _, err := fsid.FromOpen(root)
	if err != nil {
		return err
	}
	s.allowedDevices(id)
	s.walkDir(ctx, root, o.Root)
	if s.skipCount > len(s.skipped) {
		s.skipped = append(s.skipped, model.Skip{Path: "…", Reason: fmt.Sprintf("%d more skipped paths not listed", s.skipCount-len(s.skipped))})
	}
	return nil
}

// walkDir only resolves child names against held directory descriptors.
// Renaming an ancestor or replacing it with a symlink cannot redirect it.
func (s *scanner) walkDir(ctx context.Context, dir *os.File, path string) {
	if ctx.Err() != nil {
		return
	}
	s.prog.Dirs.Add(1)
	if s.prog.Dirs.Load()%64 == 0 {
		s.prog.set("scanning", path)
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		s.fail(path, err)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		child := filepath.Join(path, entry.Name())
		if entry.Type()&fs.ModeSymlink != 0 {
			continue
		}
		// O_EVTONLY permits fstat without opening file contents, including
		// cloud placeholders and special files. OpenAt rejects symlinks.
		f, err := macosfs.OpenAt(dir, entry.Name(), syscall.O_EVTONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ELOOP) {
				s.fail(child, err)
			}
			continue
		}
		id, info, err := fsid.FromOpen(f)
		f.Close()
		if err != nil {
			s.fail(child, err)
			continue
		}
		if reason := s.opts.skipReason(child, info.IsDir()); reason != "" {
			s.skip(child, reason)
			continue
		}
		if !s.opts.CrossDevices && !s.devs[id.Dev] {
			s.skip(child, "different filesystem (use --cross-devices to include)")
			continue
		}
		if info.IsDir() {
			d, err := macosfs.OpenAt(dir, entry.Name(), syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
			if err != nil {
				s.fail(child, err)
				continue
			}
			current, _, err := fsid.FromOpen(d)
			if err == nil && !current.SameFile(id) {
				err = fmt.Errorf("%w: directory was replaced", fsid.ErrChanged)
			}
			if err != nil {
				s.fail(child, err)
			} else {
				s.walkDir(ctx, d, child)
			}
			d.Close()
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		s.prog.Files.Add(1)
		s.prog.Bytes.Add(id.Size)
		st, _ := info.Sys().(*syscall.Stat_t)
		if st != nil && st.Flags&sfDataless != 0 {
			s.skip(child, "cloud placeholder (contents not downloaded)")
			continue
		}
		fi := fileInfo{Path: child, Rel: homeRel(child, s.opts.Home), ModTime: info.ModTime(), ID: id}
		if it := s.opts.Classify(fi); it != nil {
			s.items[child] = it
		}
		if s.opts.Duplicates && id.Size >= s.opts.DupThreshold {
			s.dupCands = append(s.dupCands, fi)
		}
	}
}
