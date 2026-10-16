// Package display makes untrusted filenames and errors safe for a terminal.
// Use the original strings for filesystem operations and structured output.
package display

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Text replaces terminal controls and bidirectional formatting controls with
// visible escapes. Other Unicode text is preserved. It is idempotent, so a
// string may safely pass through both layout and rendering.
func Text(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&b, "\\x%02x", s[0])
		} else if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			fmt.Fprintf(&b, "\\x%02x", r)
		} else if r == 0x061c || r == 0x200e || r == 0x200f ||
			(r >= 0x2028 && r <= 0x202e) || (r >= 0x2066 && r <= 0x206f) {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
		s = s[size:]
	}
	return b.String()
}
