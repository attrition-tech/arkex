// Package sanitize strips terminal control sequences from untrusted text.
//
// Model replies, tool output and server errors are written to the user's
// terminal. Any of them could carry an escape sequence that the terminal
// would act on: OSC 52 rewrites the clipboard, OSC 8 hides a link target,
// CSI sequences move the cursor or clear the screen. Terminal removes all
// of that and keeps only printable text plus tab and newline.
package sanitize

import "strings"

// Terminal returns s without C0 controls (except \t and \n), DEL, C1
// controls and ESC-introduced sequences (CSI, OSC, DCS, APC, PM, SOS and
// two-byte escapes). A truncated sequence at the end of s, as happens when
// a stream is split mid-escape, drops only the ESC; the remainder is then
// ordinary text and stays visible instead of being interpreted.
func Terminal(s string) string {
	if clean(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b:
			i += skipEscape(s[i:])
		case c == '\t' || c == '\n':
			b.WriteByte(c)
			i++
		case c < 0x20 || c == 0x7f:
			i++
		case c == 0xc2 && i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f:
			i += 2 // C1 control encoded as UTF-8
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// clean reports whether s has nothing Terminal would remove, so the common
// case returns the input without allocating.
func clean(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 && c != '\t' && c != '\n' || c == 0x7f || c == 0xc2 && i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f {
			return false
		}
	}
	return true
}

// skipEscape returns the length of the escape sequence at the start of s
// (s[0] is ESC). Unknown or truncated sequences consume only the ESC.
func skipEscape(s string) int {
	if len(s) < 2 {
		return 1
	}
	switch s[1] {
	case '[': // CSI: parameters 0x30–0x3f, intermediates 0x20–0x2f, final 0x40–0x7e
		for i := 2; i < len(s); i++ {
			if c := s[i]; c >= 0x40 && c <= 0x7e {
				return i + 1
			} else if c < 0x20 || c > 0x3f {
				return i // malformed: stop before the stray byte
			}
		}
		return 1
	case ']', 'P', '_', '^', 'X': // OSC, DCS, APC, PM, SOS: up to BEL or ST (ESC \)
		for i := 2; i < len(s); i++ {
			switch s[i] {
			case 0x07:
				return i + 1
			case 0x1b:
				if i+1 < len(s) && s[i+1] == '\\' {
					return i + 2
				}
				return i // a new escape begins; let the caller see it
			}
		}
		return 1
	default:
		// nF escapes: intermediates 0x20–0x2f then a final 0x30–0x7e,
		// e.g. ESC ( B. Otherwise a two-byte escape such as ESC 7.
		i := 1
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
			i++
		}
		if i < len(s) && s[i] >= 0x30 && s[i] <= 0x7e {
			return i + 1
		}
		return 1
	}
}
