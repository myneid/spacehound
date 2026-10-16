// Package tui is SpaceHound's full-screen terminal interface. The Model is
// a pure state machine (Update/View) so it can be tested without a
// terminal; Run wires it to a raw-mode TTY.
//
// Scanning and selecting never modify files. Every mutating action goes
// through a summary screen and a typed confirmation word.
package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"spacehound/internal/diskspace"
	"spacehound/internal/fsid"
	"spacehound/internal/model"
	"spacehound/internal/offload"
	"spacehound/internal/oplog"
	"spacehound/internal/term"
	"spacehound/internal/trash"
	"spacehound/internal/units"
	"spacehound/internal/volumes"
)

// Env holds the side-effecting operations the UI may invoke. All of them
// are injectable so tests and --demo never touch real data.
type Env struct {
	Trash     func(path string, id fsid.Identity) (trash.Result, error)
	Volumes   func() ([]volumes.Volume, error)
	Offloader *offload.Offloader
	FreeSpace func() (diskspace.Info, error)
	Log       *oplog.Logger
	Now       func() time.Time
}

// Msg is an input to Update.
type Msg any

// KeyMsg is a keypress.
type KeyMsg struct{ Key term.Key }

// ResizeMsg reports the terminal size.
type ResizeMsg struct{ W, H int }

// RecoveryMsg makes unfinished operations visible before accepting actions.
type RecoveryMsg struct{ Entries []oplog.Entry }

// Cmd is asynchronous work started by Update. It may call send to post
// progress and returns its final message.
type Cmd func(send func(Msg)) Msg

type mode int

const (
	modeList mode = iota
	modeHelp
	modeDetail
	modeFilter
	modeConfirmTrash
	modeVolumes
	modeConfirmOffload
	modeBusy
	modeConfirmRemove
	modeResult
)

var sortNames = []string{"size", "likely savings", "age", "path"}

// Confirmation words the user must type.
const (
	WordTrash   = "trash"
	WordOffload = "offload"
	WordRemove  = "remove"
)

// Model is the UI state.
type Model struct {
	Report *model.Report
	Env    Env
	Color  bool
	Title  string // e.g. "DEMO" badge

	items    []*model.Item
	view     []*model.Item
	cursor   int
	offset   int
	selected map[string]bool
	cat      model.Category
	risk     int // -1 = any
	sortBy   int
	filter   string

	mode     mode
	prevMode mode
	input    string
	status   string

	targets     []*model.Item
	skippedOff  []string
	vols        []volumes.Volume
	volCursor   int
	chosenVol   volumes.Volume
	verified    []offload.Result
	verifiedIt  []*model.Item
	resultTitle string
	resultLines []string
	busyTitle   string
	busyLine    string
	cancel      context.CancelFunc

	free      diskspace.Info
	freeErr   error
	freeStart int64
	expected  int64
	pendingTr int64 // bytes moved to Trash, freed only when emptied
	width     int
	height    int
	Quit      bool
}

// New creates a model for a finished report.
func New(rep *model.Report, env Env) *Model {
	m := &Model{Report: rep, Env: env, Color: true, selected: map[string]bool{}, risk: -1, width: 100, height: 30}
	m.items = append(m.items, rep.Items...)
	if rep.FreeBytes > 0 {
		m.free = diskspace.Info{Free: rep.FreeBytes, Total: rep.TotalBytes}
		m.freeStart = rep.FreeBytes
	}
	m.refresh()
	return m
}

// Init returns the initial command (a free-space refresh).
func (m *Model) Init() Cmd { return m.freeCmd() }

type freeMsg struct {
	info diskspace.Info
	err  error
	user bool
}

func (m *Model) freeCmd() Cmd {
	return m.freeCmdUser(false)
}

func (m *Model) freeCmdUser(user bool) Cmd {
	f := m.Env.FreeSpace
	if f == nil {
		return nil
	}
	return func(func(Msg)) Msg {
		info, err := f()
		return freeMsg{info, err, user}
	}
}

func (m *Model) now() time.Time {
	if m.Env.Now != nil {
		return m.Env.Now()
	}
	return time.Now()
}

// refresh recomputes the visible list from filters and sort order.
func (m *Model) refresh() {
	var cur string
	if m.cursor < len(m.view) {
		cur = m.view[m.cursor].Path
	}
	m.view = m.view[:0]
	f := strings.ToLower(m.filter)
	for _, it := range m.items {
		if m.cat != "" && !it.Has(m.cat) {
			continue
		}
		if m.risk >= 0 && int(it.Risk) != m.risk {
			continue
		}
		if f != "" && !strings.Contains(strings.ToLower(it.Path), f) {
			continue
		}
		m.view = append(m.view, it)
	}
	v := m.view
	sort.SliceStable(v, func(i, j int) bool {
		a, b := v[i], v[j]
		switch m.sortBy {
		case 1:
			if a.LikelySavings() != b.LikelySavings() {
				return a.LikelySavings() > b.LikelySavings()
			}
		case 2:
			if !a.ModTime.Equal(b.ModTime) {
				return a.ModTime.Before(b.ModTime)
			}
		case 3:
			return a.Path < b.Path
		}
		if a.Size != b.Size {
			return a.Size > b.Size
		}
		return a.Path < b.Path
	})
	m.cursor = 0
	for i, it := range m.view {
		if it.Path == cur {
			m.cursor = i
		}
	}
	m.clamp()
}

func (m *Model) clamp() {
	if m.cursor >= len(m.view) {
		m.cursor = len(m.view) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	rows := m.listRows()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+rows {
		m.offset = m.cursor - rows + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m *Model) current() *model.Item {
	if m.cursor < len(m.view) {
		return m.view[m.cursor]
	}
	return nil
}

// Selected returns the selected items in display order.
func (m *Model) Selected() []*model.Item {
	var out []*model.Item
	for _, it := range m.items {
		if m.selected[it.Path] {
			out = append(out, it)
		}
	}
	return out
}

func (m *Model) targetsOrCurrent() []*model.Item {
	if s := m.Selected(); len(s) > 0 {
		return s
	}
	if c := m.current(); c != nil {
		return []*model.Item{c}
	}
	return nil
}

func totals(items []*model.Item) (size, likely int64) {
	for _, it := range items {
		size += it.Size
		likely += it.LikelySavings()
	}
	return
}

// Update applies a message and returns follow-up work, if any.
func (m *Model) Update(msg Msg) Cmd {
	switch msg := msg.(type) {
	case RecoveryMsg:
		lines := []string{"These operations have no recorded completion. Inspect the paths before continuing; a recovery file may already have been restored or removed."}
		for _, entry := range msg.Entries {
			lines = append(lines, entry.Path, "  recovery: "+entry.RecoveryPath, "  destination: "+entry.Dest)
		}
		m.showResult("Unfinished operations", lines)
	case ResizeMsg:
		m.width, m.height = msg.W, msg.H
		m.clamp()
	case freeMsg:
		m.freeErr = msg.err
		if msg.err == nil {
			m.free = msg.info
			if m.freeStart == 0 {
				m.freeStart = msg.info.Free
			}
			if msg.user {
				m.status = "Internal free space: " + units.FormatSize(msg.info.Free)
				if s := diskspace.Shortfall(m.freeStart, msg.info.Free, m.expected); s != "" {
					m.showResult("Free space check", strings.Split(s, "\n"))
				}
			}
		}
	case progressMsg:
		m.busyLine = string(msg)
	case trashDoneMsg:
		return m.trashDone(msg)
	case volsMsg:
		m.volsLoaded(msg)
	case offloadDoneMsg:
		m.offloadDone(msg)
	case removeDoneMsg:
		m.removeDone(msg)
	case KeyMsg:
		return m.key(msg.Key)
	}
	return nil
}

func (m *Model) key(k term.Key) Cmd {
	if k == term.KeyCtrlC && m.mode != modeBusy {
		m.Quit = true
		return nil
	}
	switch m.mode {
	case modeHelp, modeDetail:
		if k == 'q' || k == term.KeyEsc || k == term.KeyLeft || k == '?' || k == term.KeyEnter || k == 'h' {
			m.mode = modeList
		}
		return nil
	case modeResult:
		m.mode = modeList
		return nil
	case modeFilter:
		switch k {
		case term.KeyEnter:
			m.mode = modeList
		case term.KeyEsc:
			m.filter = ""
			m.mode = modeList
		case term.KeyBackspace:
			if r := []rune(m.filter); len(r) > 0 {
				m.filter = string(r[:len(r)-1])
			}
		case term.KeyCtrlU:
			m.filter = ""
		default:
			if k > 0 {
				m.filter += string(rune(k))
			}
		}
		m.refresh()
		return nil
	case modeConfirmTrash, modeConfirmOffload, modeConfirmRemove:
		return m.confirmKey(k)
	case modeVolumes:
		return m.volumeKey(k)
	case modeBusy:
		if (k == term.KeyEsc || k == term.KeyCtrlC) && m.cancel != nil {
			m.cancel()
			m.busyLine = "Cancelling after the current step…"
		}
		return nil
	}
	return m.listKey(k)
}

func (m *Model) listKey(k term.Key) Cmd {
	m.status = ""
	page := m.listRows()
	switch k {
	case 'q':
		m.Quit = true
	case term.KeyUp, 'k':
		m.cursor--
	case term.KeyDown, 'j':
		m.cursor++
	case term.KeyPgUp, 'b':
		m.cursor -= page
	case term.KeyPgDn:
		m.cursor += page
	case term.KeyHome, 'g':
		m.cursor = 0
	case term.KeyEnd, 'G':
		m.cursor = len(m.view) - 1
	case ' ':
		if it := m.current(); it != nil {
			if m.selected[it.Path] {
				delete(m.selected, it.Path)
			} else {
				m.selected[it.Path] = true
			}
			m.cursor++
		}
	case 'a':
		n := 0
		for _, it := range m.view {
			if it.Risk != model.RiskDangerous {
				m.selected[it.Path] = true
				n++
			}
		}
		m.status = fmt.Sprintf("Selected %d visible items (dangerous items must be selected one by one)", n)
	case 'n':
		m.selected = map[string]bool{}
		m.status = "Selection cleared"
	case 'c', term.KeyTab:
		m.cat = nextCat(m.cat)
		m.refresh()
	case '0', '1', '2', '3', '4':
		if k == '0' {
			m.cat = ""
		} else {
			m.cat = model.Categories[k-'1']
		}
		m.refresh()
	case 'r':
		m.risk++
		if m.risk > int(model.RiskDangerous) {
			m.risk = -1
		}
		m.refresh()
	case 's':
		m.sortBy = (m.sortBy + 1) % len(sortNames)
		m.refresh()
	case '/':
		m.mode = modeFilter
	case term.KeyEnter, term.KeyRight, 'l':
		if m.current() != nil {
			m.mode = modeDetail
		}
	case '?':
		m.mode = modeHelp
	case 'f':
		m.status = "Refreshing free space…"
		return m.freeCmdUser(true)
	case 't', 'd':
		return m.startTrash()
	case 'o':
		return m.startOffload()
	}
	m.clamp()
	return nil
}

func nextCat(c model.Category) model.Category {
	if c == "" {
		return model.Categories[0]
	}
	for i, x := range model.Categories {
		if x == c && i+1 < len(model.Categories) {
			return model.Categories[i+1]
		}
	}
	return ""
}

func (m *Model) confirmKey(k term.Key) Cmd {
	switch k {
	case term.KeyEsc:
		if m.mode == modeConfirmRemove {
			m.showResult("Originals kept", append([]string{
				"Nothing was removed. The verified copies remain on " + m.chosenVol.Name + ":"},
				destLines(m.verified)...))
		} else {
			m.mode = modeList
			m.status = "Cancelled; nothing was changed"
		}
		m.input = ""
		return nil
	case term.KeyBackspace:
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
		return nil
	case term.KeyCtrlU:
		m.input = ""
		return nil
	case term.KeyEnter:
		word := strings.TrimSpace(strings.ToLower(m.input))
		m.input = ""
		switch m.mode {
		case modeConfirmTrash:
			if word == WordTrash {
				return m.runTrash()
			}
		case modeConfirmOffload:
			if word == WordOffload {
				return m.runOffload()
			}
		case modeConfirmRemove:
			if word == WordRemove || word == WordTrash {
				return m.runRemove(word == WordTrash)
			}
			if word == "" || word == "keep" {
				return m.confirmKey(term.KeyEsc)
			}
		}
		m.status = "Confirmation word did not match; nothing was changed"
		return nil
	}
	if k > 0 && len(m.input) < 32 {
		m.input += string(rune(k))
	}
	return nil
}

func (m *Model) showResult(title string, lines []string) {
	m.mode = modeResult
	m.resultTitle = title
	m.resultLines = lines
}

func (m *Model) removeItems(paths map[string]bool) {
	kept := m.items[:0]
	for _, it := range m.items {
		if !paths[it.Path] {
			kept = append(kept, it)
		}
	}
	m.items = kept
	for p := range paths {
		delete(m.selected, p)
	}
	m.refresh()
}

func (m *Model) relPath(p string) string {
	if h := m.Report.Home; h != "" && strings.HasPrefix(p, h+"/") {
		return "~/" + p[len(h)+1:]
	}
	return p
}

// dupWarnings lists duplicate groups whose every copy is among items.
func (m *Model) dupWarnings(items []*model.Item) []string {
	sel := map[string]bool{}
	for _, it := range items {
		sel[it.Path] = true
	}
	var out []string
	for _, g := range m.Report.Duplicates {
		all := sel[g.Keep]
		for _, c := range g.Copies {
			all = all && sel[c]
		}
		if all {
			out = append(out, fmt.Sprintf("WARNING: every copy of duplicate group #%d is selected; no copy would remain.", g.ID))
		}
	}
	return out
}

func riskCounts(items []*model.Item) string {
	var n [3]int
	for _, it := range items {
		n[it.Risk]++
	}
	return fmt.Sprintf("%d low, %d review, %d dangerous", n[0], n[1], n[2])
}

func plural(n int, s string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, s)
	}
	return fmt.Sprintf("%d %ss", n, s)
}

func destLines(rs []offload.Result) []string {
	var out []string
	for _, r := range rs {
		out = append(out, "  "+r.Dest)
	}
	return out
}

func (m *Model) itemPaths(items []*model.Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, fmt.Sprintf("  %-9s %10s  %s", it.Risk, units.FormatSize(it.Size), m.relPath(it.Path)))
	}
	return out
}

func base(p string) string { return filepath.Base(p) }
