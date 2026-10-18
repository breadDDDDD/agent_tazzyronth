package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Styles are SGR parameter strings (theme of plan §6.3). Text is styled with
// paint, which is a no-op when colour is off.
const (
	stAccent = "38;2;224;33;138"  // magenta #E0218A: active, selection
	stErr    = "38;2;179;18;58"   // crimson #B3123A: errors, dead
	stText   = "38;2;200;200;210" // smoky silver
	stDim    = "38;2;125;125;138" // idle, secondary
	stBold   = "1"
	stTitle  = "1;38;2;224;33;138" // bold magenta
	stSel    = "7"                 // reverse video
)

// agentColors tell agents apart in the merged timeline.
var agentColors = []string{
	"38;2;224;33;138", "38;2;90;200;220", "38;2;230;180;60", "38;2;120;210;120",
	"38;2;170;130;240", "38;2;240;130;90", "38;2;180;190;220",
}

// painter applies styles when colour is on.
type painter bool

func (p painter) paint(style, s string) string {
	if !p || style == "" || s == "" {
		return s
	}
	return "\x1b[" + style + "m" + s + "\x1b[0m"
}

// clean makes untrusted text safe for one terminal line: tabs and newlines
// become spaces, other control characters (including ESC) are dropped.
func clean(s string) string {
	ok := true
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			ok = false
			break
		}
	}
	if ok {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// runeWidth is the number of terminal cells r occupies (a small table:
// combining marks 0, East Asian wide and emoji 2, everything else 1).
func runeWidth(r rune) int {
	switch {
	case r == 0 || unicode.Is(unicode.Mn, r) || r == 0x200b || r == 0xfe0f:
		return 0
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf && r != 0x303f,
		r >= 0xac00 && r <= 0xd7a3, r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f,
		r >= 0xff00 && r <= 0xff60, r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1f64f, r >= 0x1f900 && r <= 0x1f9ff, r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

// width is the display width of s, ignoring the SGR escapes paint adds.
func width(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = skipEscape(s, i)
			continue
		}
		r, size := decodeRune(s[i:])
		n += runeWidth(r)
		i += size
	}
	return n
}

// fit truncates s to at most w cells (marking the cut with "…") and pads it
// with spaces to exactly w cells. Escapes are kept; a cut inside styled
// text is closed with a reset.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	cur := width(s)
	if cur <= w {
		return s + strings.Repeat(" ", w-cur)
	}
	var b strings.Builder
	n, styled := 0, false
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := skipEscape(s, i)
			b.WriteString(s[i:j])
			styled = s[i:j] != "\x1b[0m"
			i = j
			continue
		}
		r, size := decodeRune(s[i:])
		rw := runeWidth(r)
		if n+rw > w-1 {
			break
		}
		b.WriteRune(r)
		n += rw
		i += size
	}
	b.WriteString("…")
	n++
	if styled {
		b.WriteString("\x1b[0m")
	}
	return b.String() + strings.Repeat(" ", max(0, w-n))
}

// trunc is fit without the padding.
func trunc(s string, w int) string {
	if width(s) <= w {
		return s
	}
	return strings.TrimRight(fit(s, w), " ")
}

func skipEscape(s string, i int) int {
	j := i + 1
	if j < len(s) && s[j] == '[' {
		j++
		for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
			j++
		}
	}
	return min(j+1, len(s))
}

func decodeRune(s string) (rune, int) { return utf8.DecodeRuneInString(s) }

// cols lays out cells in fixed-width columns, each ending in a space (the
// last cell takes the rest).
func cols(widths []int, cells ...string) string {
	var b strings.Builder
	for i, c := range cells {
		if i < len(widths) {
			b.WriteString(fit(c, widths[i]-1) + " ")
		} else {
			b.WriteString(c)
		}
	}
	return b.String()
}
