package display

import "testing"

func TestTextEscapesTerminalAndDirectionControls(t *testing.T) {
	input := "holiday\x1b[2J\r\n\t\x7f\u0085\u009b\u202ephoto\u2069\u061c\u200f.jpg"
	want := `holiday\x1b[2J\x0d\x0a\x09\x7f\x85\x9b\u202ephoto\u2069\u061c\u200f.jpg`
	if got := Text(input); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := Text(want); got != want {
		t.Fatalf("sanitizing twice changed escapes: %q", got)
	}
}

func TestTextPreservesUnicode(t *testing.T) {
	want := "旅行/été/שלום/ملف/📷.jpg"
	if got := Text(want); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := Text(string([]byte{'x', 0xff})); got != `x\xff` {
		t.Fatalf("invalid filename byte lost: %q", got)
	}
}
