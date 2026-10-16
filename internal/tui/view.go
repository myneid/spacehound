package tui

import (
	"fmt"
	"strings"
	"time"

	"spacehound/internal/display"
	"spacehound/internal/model"
	"spacehound/internal/offload"
	"spacehound/internal/units"
	"spacehound/internal/volumes"
)

const (
	sReset   = "\x1b[0m"
	sBold    = "\x1b[1m"
	sDim     = "\x1b[2m"
	sInverse = "\x1b[7m"
	sRed     = "\x1b[31m"
	sGreen   = "\x1b[32m"
	sYellow  = "\x1b[33m"
	sCyan    = "\x1b[36m"
	sSelBg   = "\x1b[44;97m"
)

type seg struct{ style, text string }
type line []seg

func txt(s string) line                 { return line{{"", display.Text(s)}} }
func sty(style, s string) line          { return line{{style, display.Text(s)}} }
func (l line) add(style, s string) line { return append(l, seg{style, display.Text(s)}) }

// truncate cuts s to w runes, marking the cut with "…".
func truncate(s string, w int) string {
	s = display.Text(s)
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return string(r[:w])
	}
	return string(r[:w-1]) + "…"
}

// truncLeft keeps the end of s (useful for paths).
func truncLeft(s string, w int) string {
	s = display.Text(s)
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return string(r[len(r)-w:])
	}
	return "…" + string(r[len(r)-w+1:])
}

func pad(s string, w int) string {
	s = display.Text(s)
	n := len([]rune(s))
	if n >= w {
		return truncate(s, w)
	}
	return s + strings.Repeat(" ", w-n)
}

// wrap breaks text into lines of at most w runes at spaces.
func wrap(s string, w int) []string {
	s = display.Text(s)
	if w < 10 {
		w = 10
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		indent := para[:len(para)-len(strings.TrimLeft(para, " "))]
		words := strings.Fields(para)
		cur := indent
		for _, wd := range words {
			if len([]rune(cur))+len([]rune(wd))+1 > w && strings.TrimSpace(cur) != "" {
				out = append(out, cur)
				cur = indent + "  "
			}
			if strings.TrimSpace(cur) != "" {
				cur += " "
			}
			cur += wd
		}
		out = append(out, cur)
	}
	return out
}

func (m *Model) render(l line, w int, fill string) string {
	var b strings.Builder
	used := 0
	for _, s := range l {
		if used >= w {
			break
		}
		t := truncate(s.text, w-used)
		used += len([]rune(t))
		if m.Color && (s.style != "" || fill != "") {
			b.WriteString(sReset + fill + s.style + t + sReset)
		} else {
			b.WriteString(t)
		}
	}
	if used < w {
		if m.Color && fill != "" {
			b.WriteString(fill + strings.Repeat(" ", w-used) + sReset)
		} else {
			b.WriteString(strings.Repeat(" ", w-used))
		}
	}
	return b.String()
}

func riskStyle(r model.Risk) string {
	switch r {
	case model.RiskLow:
		return sGreen
	case model.RiskReview:
		return sYellow
	}
	return sRed + sBold
}

func (m *Model) listRows() int {
	n := m.height - 3 - detailHeight - 2
	if n < 3 {
		n = 3
	}
	return n
}

const detailHeight = 8

func age(t, now time.Time) string {
	d := int(now.Sub(t).Hours() / 24)
	switch {
	case d < 1:
		return "today"
	case d < 60:
		return fmt.Sprintf("%dd", d)
	case d < 730:
		return fmt.Sprintf("%dmo", d/30)
	}
	return fmt.Sprintf("%dy", d/365)
}

func (m *Model) refTime() time.Time {
	if m.Env.Now != nil {
		return m.Env.Now()
	}
	if !m.Report.StartedAt.IsZero() {
		return m.Report.StartedAt
	}
	return time.Now()
}

// View renders the whole screen as w x h text (with ANSI styles when Color
// is set). Lines are separated by "\r\n" so the output works in raw mode.
func (m *Model) View(w, h int) string {
	m.width, m.height = w, h
	m.clamp()
	var lines []string
	add := func(l line, fill string) { lines = append(lines, m.render(l, w, fill)) }

	add(m.titleLine(), sInverse)
	switch m.mode {
	case modeHelp:
		for _, s := range helpText {
			add(txt(" "+s), "")
		}
	case modeDetail:
		m.viewDetail(add, w, h-3)
	case modeConfirmTrash:
		m.viewConfirmTrash(add, w)
	case modeVolumes:
		m.viewVolumes(add, w)
	case modeConfirmOffload:
		m.viewConfirmOffload(add, w)
	case modeConfirmRemove:
		m.viewConfirmRemove(add, w)
	case modeBusy:
		add(txt(""), "")
		add(sty(sBold, " "+m.busyTitle+"…"), "")
		add(txt(""), "")
		add(txt(" "+m.busyLine), "")
		add(txt(""), "")
		add(sty(sDim, " Press Esc to cancel after the current step. Originals are never removed by cancelling."), "")
	case modeResult:
		add(sty(sBold, " "+m.resultTitle), "")
		add(txt(""), "")
		for _, l := range m.resultLines {
			for _, s := range wrap(l, w-2) {
				add(txt(" "+s), "")
			}
		}
		add(txt(""), "")
		add(sty(sDim, " Press any key to return to the list."), "")
	default:
		m.viewList(add, w)
	}
	// Footer: pad to h-2, then status and key help.
	maxBody := h - 2
	if len(lines) > maxBody {
		lines = lines[:maxBody]
	}
	for len(lines) < maxBody {
		lines = append(lines, m.render(txt(""), w, ""))
	}
	status := m.status
	if m.mode == modeFilter {
		status = "Filter path: " + m.filter + "▏ (Enter apply, Esc clear)"
	}
	add(sty(sCyan, " "+status), "")
	add(txt(" "+m.keyHint()), sInverse)
	return strings.Join(lines, "\r\n")
}

func (m *Model) titleLine() line {
	t := " SpaceHound"
	if m.Title != "" {
		t += " [" + m.Title + "]"
	}
	t += "  " + m.Report.Root
	free := "free space unknown"
	if m.free.Free > 0 {
		free = fmt.Sprintf("Internal disk: %s free", units.FormatSize(m.free.Free))
		if m.free.Total > 0 {
			free += " of " + units.FormatSize(m.free.Total)
		}
	}
	if m.pendingTr > 0 {
		free += fmt.Sprintf(" · %s waiting in Trash", units.FormatSize(m.pendingTr))
	}
	return txt(t + "   " + free)
}

func (m *Model) keyHint() string {
	switch m.mode {
	case modeHelp, modeDetail:
		return "Esc/←/q back"
	case modeConfirmTrash, modeConfirmOffload, modeConfirmRemove:
		return "Type the word shown, Enter to confirm, Esc to cancel"
	case modeVolumes:
		return "↑↓ choose drive  Enter use it  Esc cancel"
	case modeBusy:
		return "Esc cancel"
	case modeResult:
		return "any key: back to list"
	}
	return "↑↓/jk move  space select  a all  n none  c/0-4 category  r risk  s sort  / filter  ⏎ details  t trash  o offload  f free  ? help  q quit"
}

func (m *Model) viewList(add func(line, string), w int) {
	catName := "all"
	if m.cat != "" {
		catName = string(m.cat)
	}
	riskName := "any"
	if m.risk >= 0 {
		riskName = model.Risk(m.risk).String()
	}
	vs, _ := totals(m.view)
	sel := m.Selected()
	ss, sl := totals(sel)
	info := fmt.Sprintf(" View: %s · Risk: %s · Sort: %s", catName, riskName, sortNames[m.sortBy])
	if m.filter != "" {
		info += fmt.Sprintf(" · Filter: %q", m.filter)
	}
	info += fmt.Sprintf(" · %d items, %s", len(m.view), units.FormatSize(vs))
	selInfo := fmt.Sprintf("   Selected: %d (%s, likely frees %s)", len(sel), units.FormatSize(ss), units.FormatSize(sl))
	add(txt(info).add(sBold, selInfo), "")

	pathW := w - 46
	if pathW < 10 {
		pathW = 10
	}
	add(sty(sBold, fmt.Sprintf(" %-3s %-9s %10s %10s %5s  %-10s %s", "", "RISK", "SIZE", "SAVES", "AGE", "CATEGORY", "PATH")), "")
	rows := m.listRows()
	now := m.refTime()
	if len(m.view) == 0 {
		add(sty(sDim, "   No items match the current view. Press 0 for all categories, r to change risk, / to edit the filter."), "")
		rows--
	}
	for i := m.offset; i < m.offset+rows; i++ {
		if i >= len(m.view) {
			add(txt(""), "")
			continue
		}
		it := m.view[i]
		mark := "[ ]"
		if m.selected[it.Path] {
			mark = "[x]"
		}
		cat := string(it.Category)
		if len(it.Categories) > 1 {
			cat += "+"
		}
		fill := ""
		if i == m.cursor {
			fill = sSelBg
		}
		l := txt(" "+mark+" ").
			add(riskStyle(it.Risk), fmt.Sprintf("%-9s", it.Risk)).
			add("", fmt.Sprintf(" %10s %10s %5s  %-10s ", units.FormatSize(it.Size), units.FormatSize(it.LikelySavings()), age(it.ModTime, now), cat)).
			add("", truncLeft(m.relPath(it.Path), pathW))
		add(l, fill)
	}
	add(sty(sDim, strings.Repeat("─", w)), "")
	it := m.current()
	var d []line
	if it != nil {
		d = append(d, txt(" ").add(sBold, m.relPath(it.Path)))
		d = append(d, txt(fmt.Sprintf(" %s logical · %s allocated · modified %s · ", units.FormatSize(it.Size),
			units.FormatSize(it.Allocated), it.ModTime.Format("2006-01-02"))).add(riskStyle(it.Risk), "risk "+strings.ToUpper(it.Risk.String())))
		for _, r := range it.Reasons {
			for j, s := range wrap(r, w-4) {
				p := " • "
				if j > 0 {
					p = "   "
				}
				d = append(d, txt(p+strings.TrimLeft(s, " ")))
			}
		}
	}
	for i := 0; i < detailHeight-1; i++ {
		if i < len(d) {
			if i == detailHeight-2 && len(d) > detailHeight-1 {
				add(sty(sDim, "   … press Enter for all details"), "")
				continue
			}
			add(d[i], "")
		} else {
			add(txt(""), "")
		}
	}
}

func (m *Model) viewDetail(add func(line, string), w, h int) {
	it := m.current()
	if it == nil {
		return
	}
	now := m.refTime()
	kv := func(k, v string) { add(txt(fmt.Sprintf(" %-14s ", k)).add("", v), "") }
	add(sty(sBold, " Details"), "")
	kv("Path", it.Path)
	kv("Logical size", fmt.Sprintf("%s (%d bytes)", units.FormatSize(it.Size), it.Size))
	kv("Allocated", units.FormatSize(it.Allocated)+" on disk")
	kv("Likely saves", units.FormatSize(it.LikelySavings())+" (not guaranteed: clones, snapshots and open files can retain blocks)")
	kv("Modified", it.ModTime.Format("2006-01-02 15:04")+" ("+age(it.ModTime, now)+" ago)")
	var cats []string
	for _, c := range it.Categories {
		cats = append(cats, string(c))
	}
	kv("Categories", strings.Join(cats, ", "))
	add(txt(fmt.Sprintf(" %-14s ", "Risk")).add(riskStyle(it.Risk), strings.ToUpper(it.Risk.String())).add("", "  "+riskHelp[it.Risk]), "")
	kv("Identity", fmt.Sprintf("dev %d · inode %d · links %d (re-checked before any action)", it.Identity.Dev, it.Identity.Ino, it.Identity.Nlink))
	if why := OffloadBlock(it); why != "" {
		kv("Offload", "not offered: "+why)
	} else {
		kv("Offload", "available (o): copy + SHA-256 verify, original kept until you confirm")
	}
	add(txt(""), "")
	add(sty(sBold, " Why it was flagged"), "")
	for _, r := range it.Reasons {
		for j, s := range wrap(r, w-5) {
			p := "  • "
			if j > 0 {
				p = "    "
			}
			add(txt(p+strings.TrimLeft(s, " ")), "")
		}
	}
	if it.DupGroup > 0 {
		for _, g := range m.Report.Duplicates {
			if g.ID != it.DupGroup {
				continue
			}
			add(txt(""), "")
			add(sty(sBold, fmt.Sprintf(" Duplicate group #%d · sha256 %s", g.ID, g.SHA256[:16])), "")
			add(txt("   keep  "+m.relPath(g.Keep)), "")
			for _, c := range g.Copies {
				add(txt("   copy  "+m.relPath(c)), "")
			}
		}
	}
}

var riskHelp = map[model.Risk]string{
	model.RiskLow:       "strong evidence it is regenerable or disposable",
	model.RiskReview:    "probably your data or ambiguous; look before acting",
	model.RiskDangerous: "may break an app or lose data; manual review only",
}

func (m *Model) prompt(add func(line, string), word, what string) {
	add(txt(""), "")
	add(txt(" Type ").add(sBold+sYellow, word).add("", " and press Enter to "+what+". Esc cancels."), "")
	add(txt(" > ").add(sBold, m.input+"▏"), "")
}

func (m *Model) listTargets(add func(line, string), w int, max int) {
	for i, s := range m.itemPaths(m.targets) {
		if i == max {
			add(sty(sDim, fmt.Sprintf("  … and %d more", len(m.targets)-max)), "")
			break
		}
		add(txt(truncLeft(s, w)), "")
	}
}

func (m *Model) viewConfirmTrash(add func(line, string), w int) {
	size, likely := totals(m.targets)
	add(sty(sBold, " Move to Trash"), "")
	add(txt(""), "")
	add(txt(fmt.Sprintf(" %s, %s logical, likely frees %s once the Trash is emptied (%s).",
		plural(len(m.targets), "item"), units.FormatSize(size), units.FormatSize(likely), riskCounts(m.targets))), "")
	add(txt(" Each file's identity, size, modification/change times and link count are checked; changed files are skipped."), "")
	add(txt(" Nothing is deleted permanently. Restore manually from the Trash using the original paths in the operation log."), "")
	for _, wn := range m.dupWarnings(m.targets) {
		add(sty(sRed+sBold, " "+wn), "")
	}
	for _, it := range m.targets {
		if it.Risk == model.RiskDangerous {
			add(sty(sRed+sBold, " WARNING: dangerous items are selected; an app may break or data may be lost."), "")
			break
		}
	}
	add(txt(""), "")
	m.listTargets(add, w, 12)
	m.prompt(add, WordTrash, "move these to the Trash")
}

func (m *Model) viewVolumes(add func(line, string), w int) {
	total, _ := totals(m.targets)
	add(sty(sBold, fmt.Sprintf(" Offload %s (%s) to an external drive", plural(len(m.targets), "file"), units.FormatSize(total))), "")
	if len(m.skippedOff) > 0 {
		add(sty(sYellow, fmt.Sprintf(" %s not offered:", plural(len(m.skippedOff), "item"))), "")
		for _, s := range m.skippedOff {
			add(txt(s), "")
		}
	}
	add(txt(""), "")
	if len(m.vols) == 0 {
		add(txt(" No volumes are mounted under /Volumes. Connect a drive and try again."), "")
		return
	}
	for i, v := range m.vols {
		fill := ""
		if i == m.volCursor {
			fill = sSelBg
		}
		state := "OK"
		st := sGreen
		if !v.Suitable {
			state, st = "unsuitable", sRed
		}
		wr := "read-only"
		if v.Writable {
			wr = "writable"
		}
		add(txt(fmt.Sprintf("  %-24s %-6s %-9s %10s free of %-10s ", truncate(v.Name, 24), v.FSType, wr,
			units.FormatSize(v.Free), units.FormatSize(v.Total))).add(st, state), fill)
		if !v.Suitable {
			add(sty(sDim, "      "+strings.Join(v.Reasons, "; ")), "")
		} else if err := v.CanHold(total); err != nil {
			add(sty(sYellow, "      "+err.Error()), "")
		}
	}
	add(txt(""), "")
	add(sty(sDim, fmt.Sprintf(" Destination: <drive>/%s/<host>/<date>/<path relative to home>. %s headroom is kept free.",
		"SpaceHound Archive", "1% (min 100 MiB)")), "")
}

func (m *Model) viewConfirmOffload(add func(line, string), w int) {
	total, likely := totals(m.targets)
	v := m.chosenVol
	add(sty(sBold, " Offload to "+v.Name), "")
	add(txt(""), "")
	add(txt(fmt.Sprintf(" %s, %s. %s has %s free.", plural(len(m.targets), "file"), units.FormatSize(total), v.Name, units.FormatSize(v.Free))), "")
	add(txt(" Steps: native copy of contents and metadata → verify both completely → re-check the original."), "")
	add(txt(" Existing files on the drive are never overwritten. Originals stay until you confirm again."), "")
	add(txt(fmt.Sprintf(" Removing the originals afterwards would likely free %s on the internal disk.", units.FormatSize(likely))), "")
	add(txt(""), "")
	for i, it := range m.targets {
		if i == 8 {
			add(sty(sDim, fmt.Sprintf("  … and %d more", len(m.targets)-8)), "")
			break
		}
		dest, err := m.Env.Offloader.Plan(offloadReq(it, v))
		if err != nil {
			dest = "cannot plan: " + err.Error()
		}
		add(txt("  "+truncLeft(m.relPath(it.Path), w-4)), "")
		add(sty(sDim, "    → "+truncLeft(dest, w-8)), "")
	}
	m.prompt(add, WordOffload, "copy and verify")
}

func (m *Model) viewConfirmRemove(add func(line, string), w int) {
	var size int64
	for _, r := range m.verified {
		size += r.Size
	}
	add(sty(sBold+sGreen, fmt.Sprintf(" Copy verified for %s (%s)", plural(len(m.verified), "file"), units.FormatSize(size))), "")
	add(txt(""), "")
	for i, r := range m.verified {
		if i == 6 {
			add(sty(sDim, fmt.Sprintf("  … and %d more", len(m.verified)-6)), "")
			break
		}
		add(txt("  "+truncLeft(m.relPath(r.Source), w-4)), "")
		add(sty(sDim, "    → "+truncLeft(r.Dest, w-8)), "")
		add(sty(sDim, "      sha256 "+r.SHA256), "")
	}
	for _, f := range m.resultLines {
		add(sty(sYellow, " "+f), "")
	}
	add(txt(""), "")
	add(txt(" The originals are still on the internal disk. Choose what to do with them:"), "")
	add(sty(sYellow, " Close apps that may write these files before removing originals; in-flight writes can escape detection."), "")
	add(txt("   ").add(sBold+sYellow, WordRemove).add("", "  delete the originals now (the verified copies on "+m.chosenVol.Name+" remain)"), "")
	add(txt("   ").add(sBold+sYellow, WordTrash).add("", "   move the originals to the Trash instead"), "")
	add(txt("   Esc     keep both (nothing else changes)"), "")
	add(txt(" > ").add(sBold, m.input+"▏"), "")
}

var helpText = []string{
	"",
	"SpaceHound finds large files, duplicates, caches and stale logs. Scanning and",
	"selecting never change anything; every action needs a typed confirmation.",
	"",
	"Navigation   ↑/↓ or j/k move · PgUp/PgDn or b · g/G first/last · Enter/→ details",
	"Selection    space toggle · a select visible (except dangerous) · n clear",
	"Views        c or Tab cycle category · 0 all · 1 large · 2 duplicate · 3 cache · 4 stale-log",
	"             r cycle risk filter · s cycle sort · / filter by path text",
	"Actions      t (or d) move selected (or current) to the Trash, after typing \"trash\"",
	"             o offload selected regular files to an external drive: copy, verify",
	"               SHA-256, then separately confirm removing the originals",
	"             f refresh internal free space (explains missing space if needed)",
	"Other        ? this help · q quit",
	"",
	"Risk levels  low: regenerable or disposable with strong evidence",
	"             review: probably your data or ambiguous; look first",
	"             dangerous: app data, databases, VM images; manual review only",
	"",
	"SAVES is a conservative estimate: allocated bytes, zero for hard-linked files.",
	"APFS clones, snapshots and open files can keep space in use after removal.",
}

func offloadReq(it *model.Item, v volumes.Volume) offload.Request {
	return offload.Request{Source: it.Path, Identity: it.Identity, Volume: v}
}
