package tui

import (
	"errors"
	"strings"
	"testing"

	"spacehound/internal/fsid"
	"spacehound/internal/model"
	"spacehound/internal/term"
	"spacehound/internal/trash"
)

func TestTrashNeedsTypedConfirmation(t *testing.T) {
	calls := 0
	item := &model.Item{Path: "/fixture/selected.bin", Risk: model.RiskReview}
	m := New(&model.Report{Items: []*model.Item{item}}, Env{Trash: func(string, fsid.Identity) (trash.Result, error) {
		calls++
		return trash.Result{Method: "fixture Trash"}, nil
	}})
	if cmd := m.key('t'); cmd != nil || calls != 0 {
		t.Fatal("mutation before confirmation")
	}
	for _, k := range "trahs" {
		m.key(term.Key(k))
	}
	if cmd := m.key(term.KeyEnter); cmd != nil || calls != 0 {
		t.Fatal("typo authorized mutation")
	}
	for _, k := range "trash" {
		m.key(term.Key(k))
	}
	cmd := m.key(term.KeyEnter)
	if cmd == nil || calls != 0 {
		t.Fatal("confirmation did not prepare action")
	}
	cmd(func(Msg) {})
	if calls != 1 {
		t.Fatalf("confirmed action called %d times", calls)
	}
}

func TestTrashFailureCannotInjectResultRowsOrClaimSourceInPlace(t *testing.T) {
	item := &model.Item{Path: "/fixture/evil\nFORGED.bin", Risk: model.RiskReview}
	m := New(&model.Report{Items: []*model.Item{item}}, Env{Trash: func(string, fsid.Identity) (trash.Result, error) {
		return trash.Result{}, errors.New("restoration blocked\nFORGED RESULT")
	}})
	m.startTrash()
	msg := m.runTrash()(func(Msg) {}).(trashDoneMsg)
	if lines := splitAll(msg.fails); len(lines) != 2 {
		t.Fatalf("filename/error injected extra rows: %q", lines)
	}
	m.trashDone(msg)
	joined := strings.Join(m.resultLines, "\n")
	if strings.Contains(joined, "were left in place") || !strings.Contains(joined, "need inspection") || !strings.Contains(joined, `\x0a`) {
		t.Fatalf("unsafe or inaccurate failure report: %q", joined)
	}
}
