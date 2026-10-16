package tui

import (
	"strings"
	"testing"

	"spacehound/internal/model"
)

func TestRenderEscapesUntrustedTextBeforeLayout(t *testing.T) {
	m := &Model{Color: false}
	input := "A\x1b[2JB\r\n\u202eC"
	got := m.render(line{{text: input}}, 100, "")
	if strings.ContainsAny(got, "\x1b\r\n\u202e") || !strings.Contains(got, `A\x1b[2JB\x0d\x0a\u202eC`) {
		t.Fatalf("unsafe rendering: %q", got)
	}
	if got := truncate("\x1b[2J", 5); got != `\x1b…` {
		t.Fatalf("truncation measured raw controls: %q", got)
	}
	if got := truncLeft("\x1b[2J", 7); got != `\x1b[2J` {
		t.Fatalf("left truncation measured raw controls: %q", got)
	}
	for _, line := range wrap("one\n\x1b[2Jtwo", 100) {
		if strings.ContainsAny(line, "\x1b\n") {
			t.Fatalf("wrap treated untrusted controls as layout: %q", line)
		}
	}
}

func TestViewNoColorCannotEmitFilenameTerminalControls(t *testing.T) {
	path := "/fixture/evil\x1b[2J\r\n\u202efile.bin"
	rep := &model.Report{Root: "/fixture", Home: "/fixture", Items: []*model.Item{{Path: path, Size: 100, Risk: model.RiskReview}}}
	m := New(rep, Env{})
	m.Color = false
	m.mode = modeDetail
	m.status = "error in " + path
	got := m.View(200, 30)
	if strings.ContainsAny(got, "\x1b\u202e") || !strings.Contains(got, `evil\x1b[2J\x0d\x0a\u202efile.bin`) {
		t.Fatalf("unsafe detail rendering: %q", got)
	}
	for _, line := range strings.Split(got, "\r\n") {
		if strings.ContainsAny(line, "\r\n") {
			t.Fatalf("filename injected screen rows: %q", line)
		}
	}
}
