package scan

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"spacehound/internal/fsid"
	"spacehound/internal/macosfs"
	"spacehound/internal/model"
	"spacehound/internal/units"
)

const sampleChunk = 64 * 1024

// hashers is indirected so tests can count calls.
type hashers struct {
	sample func(ctx context.Context, path string, size int64) (string, error)
	full   func(ctx context.Context, path string, prog *Progress) (string, error)
}

var defaultHashers = hashers{sample: SampleHash, full: FullHash}

type ctxReader struct {
	ctx  context.Context
	r    io.Reader
	prog *Progress
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	if c.prog != nil {
		c.prog.HashDone.Add(int64(n))
	}
	return n, err
}

// SampleHash hashes the size plus three 64 KiB chunks (start, middle, end).
// Files that differ here cannot be identical; files that match still need a
// full hash.
func SampleHash(ctx context.Context, path string, size int64) (string, error) {
	f, id, err := openHashFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if id.Size != size {
		return "", fmt.Errorf("%w: size changed before sampling", fsid.ErrChanged)
	}
	h := sha256.New()
	var sz [8]byte
	binary.LittleEndian.PutUint64(sz[:], uint64(size))
	h.Write(sz[:])
	buf := make([]byte, sampleChunk)
	for _, off := range []int64{0, size/2 - sampleChunk/2, size - sampleChunk} {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if off < 0 {
			off = 0
		}
		n, err := f.ReadAt(buf, off)
		if err != nil && err != io.EOF {
			return "", err
		}
		h.Write(buf[:n])
	}
	if err := validateHashFile(f, path, id); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FullHash returns the SHA-256 of the entire file.
func FullHash(ctx context.Context, path string, prog *Progress) (string, error) {
	f, id, err := openHashFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx: ctx, r: f, prog: prog}); err != nil {
		return "", err
	}
	if err := validateHashFile(f, path, id); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// openHashFile rejects symlinks in every component and checks regular-file
// type and cloud flags before obtaining a descriptor that can read contents.
func openHashFile(path string) (*os.File, fsid.Identity, error) {
	dir, err := macosfs.OpenDir(filepath.Dir(path), false, 0)
	if err != nil {
		return nil, fsid.Identity{}, err
	}
	defer dir.Close()
	name := filepath.Base(path)
	stat, err := macosfs.OpenAt(dir, name, syscall.O_EVTONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fsid.Identity{}, err
	}
	id, info, err := fsid.FromOpen(stat)
	stat.Close()
	if err != nil {
		return nil, fsid.Identity{}, err
	}
	if err := fsid.Check(info, id); err != nil {
		return nil, fsid.Identity{}, err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Flags&sfDataless != 0 {
		return nil, fsid.Identity{}, fmt.Errorf("cloud placeholder (contents not downloaded)")
	}
	f, err := macosfs.OpenAt(dir, name, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fsid.Identity{}, err
	}
	_, info, err = fsid.FromOpen(f)
	if err == nil {
		err = fsid.Check(info, id)
	}
	if err != nil {
		f.Close()
		return nil, fsid.Identity{}, err
	}
	return f, id, nil
}

func validateHashFile(f *os.File, path string, want fsid.Identity) error {
	_, info, err := fsid.FromOpen(f)
	if err == nil {
		err = fsid.Check(info, want)
	}
	if err != nil {
		return err
	}
	current, _, err := openHashFile(path)
	if err != nil {
		return err
	}
	defer current.Close()
	_, info, err = fsid.FromOpen(current)
	if err != nil {
		return err
	}
	return fsid.Check(info, want)
}

type hashedGroup struct {
	hash  string
	files []fileInfo
}

// groupDuplicates finds sets of identical files among candidates:
//  1. hard-link aliases (same device+inode) collapse to one entry, because
//     deleting an alias frees nothing;
//  2. files are grouped by logical size;
//  3. same-size files are compared by a sampled hash;
//  4. surviving candidates get a full SHA-256, and are re-checked for
//     modification during hashing.
func (s *scanner) groupDuplicates(ctx context.Context, cands []fileInfo, hs hashers) []hashedGroup {
	seen := map[[2]uint64]bool{}
	bySize := map[int64][]fileInfo{}
	for _, f := range cands {
		key := [2]uint64{f.ID.Dev, f.ID.Ino}
		if seen[key] {
			continue
		}
		seen[key] = true
		bySize[f.ID.Size] = append(bySize[f.ID.Size], f)
	}
	sizes := make([]int64, 0, len(bySize))
	for sz, fs := range bySize {
		if len(fs) > 1 {
			sizes = append(sizes, sz)
		}
	}
	sort.Slice(sizes, func(i, j int) bool { return sizes[i] > sizes[j] })

	regroup := func(files []fileInfo, hash func(fileInfo) (string, error)) map[string][]fileInfo {
		out := map[string][]fileInfo{}
		for _, f := range files {
			if ctx.Err() != nil {
				return nil
			}
			s.prog.set("comparing duplicates", f.Path)
			h, err := hash(f)
			if err != nil {
				if ctx.Err() == nil {
					s.fail(f.Path, err)
				}
				continue
			}
			out[h] = append(out[h], f)
		}
		return out
	}

	var result []hashedGroup
	for _, sz := range sizes {
		samples := regroup(bySize[sz], func(f fileInfo) (string, error) {
			if err := fsid.Revalidate(f.Path, f.ID); err != nil {
				return "", err
			}
			return hs.sample(ctx, f.Path, f.ID.Size)
		})
		var need []fileInfo
		for _, g := range samples {
			if len(g) > 1 {
				need = append(need, g...)
			}
		}
		if len(need) == 0 {
			continue
		}
		s.prog.HashTotal.Add(sz * int64(len(need)))
		fulls := regroup(need, func(f fileInfo) (string, error) {
			h, err := hs.full(ctx, f.Path, s.prog)
			if err != nil {
				return "", err
			}
			if err := fsid.Revalidate(f.Path, f.ID); err != nil {
				return "", err
			}
			return h, nil
		})
		for h, g := range fulls {
			if len(g) > 1 {
				result = append(result, hashedGroup{hash: h, files: g})
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.files[0].ID.Size != b.files[0].ID.Size {
			return a.files[0].ID.Size > b.files[0].ID.Size
		}
		return a.hash < b.hash
	})
	return result
}

// chooseKeeper orders a group so the copy to keep comes first: copies inside
// application data are kept (removing them could break the app), then the
// oldest file, then the shortest path.
func chooseKeeper(files []fileInfo) {
	sort.SliceStable(files, func(i, j int) bool {
		si, sj := sensitivity(files[i].Path, files[i].Rel) != "", sensitivity(files[j].Path, files[j].Rel) != ""
		if si != sj {
			return si
		}
		if !files[i].ModTime.Equal(files[j].ModTime) {
			return files[i].ModTime.Before(files[j].ModTime)
		}
		if len(files[i].Path) != len(files[j].Path) {
			return len(files[i].Path) < len(files[j].Path)
		}
		return files[i].Path < files[j].Path
	})
}

func (s *scanner) findDuplicates(ctx context.Context) []model.DuplicateGroup {
	groups := s.groupDuplicates(ctx, s.dupCands, defaultHashers)
	if ctx.Err() != nil {
		return nil
	}
	var out []model.DuplicateGroup
	for i, g := range groups {
		chooseKeeper(g.files)
		keep := g.files[0]
		dg := model.DuplicateGroup{ID: i + 1, Size: keep.ID.Size, SHA256: g.hash, Keep: keep.Path}
		for _, f := range g.files[1:] {
			dg.Copies = append(dg.Copies, f.Path)
			dg.Recoverable += f.ID.Size
			it := s.items[f.Path]
			if it == nil {
				it = &model.Item{Path: f.Path, Size: f.ID.Size, Allocated: f.ID.Allocated, ModTime: f.ModTime, Identity: f.ID}
				s.items[f.Path] = it
			}
			risk := model.RiskReview
			reason := fmt.Sprintf("Duplicate #%d: identical SHA-256 to %s (keeping that copy: %s)",
				dg.ID, keep.Path, keeperWhy(g.files))
			if sens := sensitivity(f.Path, f.Rel); sens != "" {
				risk = model.RiskDangerous
				reason += "; but this copy is " + sens
			}
			it.Add(model.CatDuplicate, risk, reason)
			it.DupGroup = dg.ID
			if len(it.Categories) == 1 {
				s.opts.annotate(it)
				it.Reasons = append(it.Reasons, "APFS clones share blocks invisibly; if these copies are clones, removing one frees little")
			}
		}
		if it := s.items[keep.Path]; it != nil {
			it.DupGroup = dg.ID
			it.Reasons = append(it.Reasons, fmt.Sprintf("Has %d identical copies (duplicate group #%d); this is the copy SpaceHound suggests keeping",
				len(g.files)-1, dg.ID))
		}
		out = append(out, dg)
	}
	return out
}

func keeperWhy(sorted []fileInfo) string {
	k := sorted[0]
	if sensitivity(k.Path, k.Rel) != "" {
		return "it is inside application data"
	}
	if len(sorted) > 1 && k.ModTime.Before(sorted[1].ModTime) {
		return "oldest"
	}
	return "shortest path"
}

// FormatGroup is a one-line description used by text output.
func FormatGroup(g model.DuplicateGroup) string {
	return fmt.Sprintf("#%d %s x%d, recoverable %s", g.ID, units.FormatSize(g.Size), len(g.Copies)+1, units.FormatSize(g.Recoverable))
}
