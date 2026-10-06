// Package textutil holds small helpers for text that came from somewhere we
// do not control (the cluster, the model) before it reaches the terminal.
package textutil

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sanitize makes s safe to print: it removes ANSI/OSC escape sequences and
// other control characters, so a log line or a model reply cannot move the
// cursor, recolour the approval dialog, set the window title or write to the
// clipboard. Newlines are kept; tabs become spaces.
func Sanitize(s string) string {
	if !needsSanitize(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteRune(utf8.RuneError)
		case r == '\n':
			b.WriteByte('\n')
		case r == '\t':
			b.WriteString("    ")
		case r == 0x1b:
			i += skipEscape(s[i:])
			continue
		case r == 0x9b || r == 0x9d || r == 0x90: // C1 CSI / OSC / DCS
			// drop the introducer; the rest prints as harmless text
		case unicode.IsControl(r):
			// drop
		case isBidi(r):
			// bidi overrides can make text display in a misleading order
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

func needsSanitize(s string) bool {
	for _, r := range s {
		if r == '\n' {
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == utf8.RuneError || isBidi(r) {
			return true
		}
	}
	return false
}

// isBidi reports directional marks, embeddings, overrides and isolates.
func isBidi(r rune) bool {
	return r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// skipEscape returns the length of the escape sequence at the start of s
// (which begins with ESC).
func skipEscape(s string) int {
	if len(s) < 2 {
		return 1
	}
	switch s[1] {
	case '[': // CSI: parameters, intermediates, one final byte 0x40–0x7e
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
			if s[i] < 0x20 || s[i] > 0x3f {
				return i // malformed: drop the introducer, keep the text
			}
		}
		return len(s)
	case ']', 'P', '_', '^', 'X': // OSC/DCS/APC/PM/SOS: until BEL or ST
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			if s[i] == '\n' {
				return i
			}
		}
		return len(s)
	default:
		return 2
	}
}

// Truncate shortens s to at most max bytes on a rune boundary, saying how
// much was cut.
func Truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("… (%d more bytes)", len(s)-cut)
}

// TruncateHead keeps the last max bytes of s (for logs, where the end is what
// matters), starting on a line boundary when it can.
func TruncateHead(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := len(s) - max
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	if nl := strings.IndexByte(s[cut:], '\n'); nl >= 0 && nl < 4096 {
		cut += nl + 1
	}
	return fmt.Sprintf("(%d earlier bytes omitted) …\n", cut) + s[cut:]
}

// OneLine collapses whitespace runs, for values shown inline.
func OneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
