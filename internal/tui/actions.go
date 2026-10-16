package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"spacehound/internal/diskspace"
	"spacehound/internal/display"
	"spacehound/internal/model"
	"spacehound/internal/offload"

	"spacehound/internal/term"
	"spacehound/internal/units"
	"spacehound/internal/volumes"
)

type progressMsg string

type trashDoneMsg struct {
	done          []*model.Item
	lines         []string
	fails         []string
	before, after int64
}

type volsMsg struct {
	vols []volumes.Volume
	err  error
}

type offloadDoneMsg struct {
	verified []offload.Result
	items    []*model.Item
	fails    []string
}

type removeDoneMsg struct {
	removed       []*model.Item
	trashed       bool
	fails         []string
	before, after int64
}

func freeNow(env Env) int64 {
	if env.FreeSpace == nil {
		return -1
	}
	info, err := env.FreeSpace()
	if err != nil {
		return -1
	}
	return info.Free
}

func freeLine(before, after int64) string {
	if before < 0 || after < 0 {
		return "Internal free space: unavailable"
	}
	return fmt.Sprintf("Internal free space: %s before, %s after (%+.1f MiB)",
		units.FormatSize(before), units.FormatSize(after), float64(after-before)/float64(units.MiB))
}

// throttle limits progress messages to a few per second.
func throttle(send func(Msg)) func(string) {
	var last time.Time
	return func(s string) {
		if time.Since(last) >= 100*time.Millisecond {
			last = time.Now()
			send(progressMsg(s))
		}
	}
}

func (m *Model) busy(title string) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mode = modeBusy
	m.busyTitle = title
	m.busyLine = ""
	return ctx
}

// ---- Trash ----

func (m *Model) startTrash() Cmd {
	if m.Env.Trash == nil {
		m.status = "Trash is not available in this mode"
		return nil
	}
	m.targets = m.targetsOrCurrent()
	if len(m.targets) == 0 {
		m.status = "Nothing to move to the Trash"
		return nil
	}
	m.input = ""
	m.mode = modeConfirmTrash
	return nil
}

func (m *Model) runTrash() Cmd {
	targets := append([]*model.Item(nil), m.targets...)
	env := m.Env
	ctx := m.busy("Moving to Trash")
	cancel := m.cancel
	return func(send func(Msg)) Msg {
		defer cancel()
		out := trashDoneMsg{before: freeNow(env)}
		for i, it := range targets {
			if ctx.Err() != nil {
				out.fails = append(out.fails, "Cancelled; left in place: "+display.Text(it.Path))
				continue
			}
			send(progressMsg(fmt.Sprintf("%d/%d  %s", i+1, len(targets), it.Path)))
			res, err := env.Trash(it.Path, it.Identity)
			if err != nil {
				out.fails = append(out.fails, fmt.Sprintf("Trash operation result: %s\n    %s", display.Text(it.Path), display.Text(err.Error())))
			} else {
				out.done = append(out.done, it)
				out.lines = append(out.lines, fmt.Sprintf("  %s\n    -> %s", display.Text(it.Path), display.Text(res.Method)))
			}
		}
		out.after = freeNow(env)
		return out
	}
}

func (m *Model) trashDone(msg trashDoneMsg) Cmd {
	m.cancel = nil
	paths := map[string]bool{}
	for _, it := range msg.done {
		paths[it.Path] = true
	}
	_, likely := totals(msg.done)
	m.pendingTr += likely
	m.expected += likely
	m.removeItems(paths)
	lines := []string{fmt.Sprintf("Moved %s to the Trash (%s).", plural(len(msg.done), "item"), units.FormatSize(likely))}
	lines = append(lines, splitAll(msg.lines)...)
	if len(msg.fails) > 0 {
		lines = append(lines, "", fmt.Sprintf("%s need inspection:", plural(len(msg.fails), "operation")))
		lines = append(lines, splitAll(msg.fails)...)
	}
	lines = append(lines, "", freeLine(msg.before, msg.after))
	if len(msg.done) > 0 {
		lines = append(lines,
			"Items in the Trash still use disk space. Empty the Trash in Finder to reclaim",
			fmt.Sprintf("about %s, then press f to re-check free space.", units.FormatSize(likely)),
			"Restore manually from the Trash locations shown above.")
	}
	if m.Env.Log != nil {
		lines = append(lines, "Logged to "+m.Env.Log.Path())
	}
	m.showResult("Trash", lines)
	if msg.after >= 0 {
		m.free.Free = msg.after
	}
	return nil
}

// ---- Offload ----

// OffloadBlock explains why an item cannot be offloaded, or returns "".
func OffloadBlock(it *model.Item) string {
	switch {
	case it.Risk == model.RiskDangerous:
		return "app data, database or VM image; offload it manually after quitting the app"
	case it.Identity.Nlink > 1:
		return "has other hard links; offloading frees nothing"
	}
	return ""
}

func (m *Model) startOffload() Cmd {
	if m.Env.Offloader == nil || m.Env.Volumes == nil {
		m.status = "Offload is not available in this mode"
		return nil
	}
	m.targets, m.skippedOff = nil, nil
	for _, it := range m.targetsOrCurrent() {
		if why := OffloadBlock(it); why != "" {
			m.skippedOff = append(m.skippedOff, fmt.Sprintf("  %s: %s", m.relPath(it.Path), why))
			continue
		}
		m.targets = append(m.targets, it)
	}
	if len(m.targets) == 0 {
		if len(m.skippedOff) > 0 {
			m.showResult("Offload not possible", append([]string{"None of the chosen items can be offloaded:"}, m.skippedOff...))
		} else {
			m.status = "Nothing to offload"
		}
		return nil
	}
	list := m.Env.Volumes
	m.busy("Looking for external drives")
	return func(func(Msg)) Msg {
		v, err := list()
		return volsMsg{v, err}
	}
}

func (m *Model) volsLoaded(msg volsMsg) {
	m.cancel = nil
	if msg.err != nil {
		m.showResult("External drives", []string{"Could not list /Volumes: " + msg.err.Error()})
		return
	}
	m.vols = msg.vols
	m.volCursor = 0
	m.mode = modeVolumes
}

func (m *Model) volumeKey(k term.Key) Cmd {
	switch k {
	case term.KeyEsc, 'q':
		m.mode = modeList
		m.status = "Offload cancelled; nothing was changed"
	case term.KeyUp, 'k':
		if m.volCursor > 0 {
			m.volCursor--
		}
	case term.KeyDown, 'j':
		if m.volCursor < len(m.vols)-1 {
			m.volCursor++
		}
	case term.KeyEnter:
		if m.volCursor >= len(m.vols) {
			return nil
		}
		v := m.vols[m.volCursor]
		if !v.Suitable {
			m.status = v.Name + " cannot be used: " + strings.Join(v.Reasons, "; ")
			return nil
		}
		total, _ := totals(m.targets)
		for _, it := range m.targets {
			if v.MaxFileSize > 0 && it.Size > v.MaxFileSize {
				m.status = fmt.Sprintf("%s cannot store %s (%s limit on %s)", v.Name, base(it.Path), units.FormatSize(v.MaxFileSize), v.FSType)
				return nil
			}
		}
		if err := v.CanHold(total); err != nil {
			m.status = err.Error()
			return nil
		}
		m.chosenVol = v
		m.input = ""
		m.mode = modeConfirmOffload
	}
	return nil
}

func (m *Model) runOffload() Cmd {
	targets := append([]*model.Item(nil), m.targets...)
	env, vol := m.Env, m.chosenVol
	ctx := m.busy("Offloading to " + vol.Name)
	cancel := m.cancel
	return func(send func(Msg)) Msg {
		defer cancel()
		var out offloadDoneMsg
		p := throttle(send)
		for i, it := range targets {
			if ctx.Err() != nil {
				out.fails = append(out.fails, "Cancelled; not copied: "+display.Text(it.Path))
				continue
			}
			prefix := fmt.Sprintf("%d/%d  %s", i+1, len(targets), base(it.Path))
			send(progressMsg(prefix))
			res, err := env.Offloader.Offload(ctx, offload.Request{Source: it.Path, Identity: it.Identity, Volume: vol},
				func(phase string, done, total int64) {
					pct := 0.0
					if total > 0 {
						pct = 100 * float64(done) / float64(total)
					}
					p(fmt.Sprintf("%s  %s %5.1f%%  %s", prefix, phase, pct, bar(pct, 30)))
				})
			if err != nil {
				out.fails = append(out.fails, fmt.Sprintf("NOT offloaded: %s\n    %s", display.Text(it.Path), display.Text(err.Error())))
			} else {
				out.verified = append(out.verified, res)
				out.items = append(out.items, it)
			}
		}
		return out
	}
}

func bar(pct float64, w int) string {
	n := int(pct / 100 * float64(w))
	if n > w {
		n = w
	}
	if n < 0 {
		n = 0
	}
	return "[" + strings.Repeat("#", n) + strings.Repeat(".", w-n) + "]"
}

func (m *Model) offloadDone(msg offloadDoneMsg) {
	m.cancel = nil
	m.verified, m.verifiedIt = msg.verified, msg.items
	if len(msg.verified) == 0 {
		lines := []string{"No file was copied successfully. All originals were kept."}
		lines = append(lines, splitAll(msg.fails)...)
		m.showResult("Offload failed", lines)
		return
	}
	m.resultLines = splitAll(msg.fails)
	m.input = ""
	m.mode = modeConfirmRemove
}

func (m *Model) runRemove(useTrash bool) Cmd {
	results := append([]offload.Result(nil), m.verified...)
	items := append([]*model.Item(nil), m.verifiedIt...)
	env := m.Env
	title := "Removing verified originals"
	if useTrash {
		title = "Moving verified originals to the Trash"
	}
	ctx := m.busy(title)
	cancel := m.cancel
	return func(send func(Msg)) Msg {
		defer cancel()
		out := removeDoneMsg{trashed: useTrash, before: freeNow(env)}
		for i, r := range results {
			if ctx.Err() != nil {
				out.fails = append(out.fails, "Cancelled; original kept: "+display.Text(r.Source))
				continue
			}
			send(progressMsg(fmt.Sprintf("%d/%d  %s", i+1, len(results), r.Source)))
			var err error
			if useTrash {
				if err = offload.CheckRemovableContext(ctx, r); err == nil {
					_, err = env.Trash(r.Source, r.SourceIdentity)
				}
			} else {
				err = offload.RemoveSourceContext(ctx, r)
			}
			if err != nil {
				out.fails = append(out.fails, fmt.Sprintf("Removal could not complete: %s\n    %s", display.Text(r.Source), display.Text(err.Error())))
			} else {
				out.removed = append(out.removed, items[i])
			}
		}
		out.after = freeNow(env)
		return out
	}
}

func (m *Model) removeDone(msg removeDoneMsg) {
	m.cancel = nil
	paths := map[string]bool{}
	for _, it := range msg.removed {
		paths[it.Path] = true
	}
	_, likely := totals(msg.removed)
	m.removeItems(paths)
	verb := "Removed"
	if msg.trashed {
		verb = "Moved to the Trash"
	}
	lines := []string{fmt.Sprintf("%s %s from the internal disk (%s). Verified copies are on %s:",
		verb, plural(len(msg.removed), "original"), units.FormatSize(likely), m.chosenVol.Name)}
	for _, r := range m.verified {
		if paths[r.Source] {
			lines = append(lines, "  "+r.Dest, "    sha256 "+r.SHA256)
		}
	}
	if len(msg.fails) > 0 {
		lines = append(lines, "")
		lines = append(lines, splitAll(msg.fails)...)
	}
	lines = append(lines, "", freeLine(msg.before, msg.after))
	m.expected += likely
	if msg.trashed {
		m.pendingTr += likely
		lines = append(lines, "Empty the Trash in Finder to reclaim the space, then press f to re-check.")
	} else if msg.before >= 0 && msg.after >= 0 {
		if s := diskspace.Shortfall(msg.before, msg.after, likely); s != "" {
			lines = append(lines, "")
			lines = append(lines, strings.Split(s, "\n")...)
		}
	}
	if m.Env.Log != nil {
		lines = append(lines, "Logged to "+m.Env.Log.Path())
	}
	if msg.after >= 0 {
		m.free.Free = msg.after
	}
	m.verified, m.verifiedIt = nil, nil
	m.showResult("Offload complete", lines)
}

func splitAll(ss []string) []string {
	var out []string
	for _, s := range ss {
		out = append(out, strings.Split(s, "\n")...)
	}
	return out
}
