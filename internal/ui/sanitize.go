package ui

import (
	"strings"
	"unicode"
)

type sanitizeState uint8

const (
	sanitizeText sanitizeState = iota
	sanitizeEscape
	sanitizeCSI
	sanitizeOSC
	sanitizeString
)

// terminalSanitizer removes terminal control sequences while retaining normal
// Unicode text, newlines, and tabs. State is retained so escape sequences split
// across streamed text deltas cannot inject their trailing bytes.
type terminalSanitizer struct {
	state        sanitizeState
	stringEscape bool
}

func (s *terminalSanitizer) clean(text string) string {
	var cleaned strings.Builder
	for _, r := range text {
		switch s.state {
		case sanitizeText:
			switch r {
			case '\x1b':
				s.state = sanitizeEscape
			case '\u009b':
				s.state = sanitizeCSI
			case '\u009d':
				s.state = sanitizeOSC
			case '\u0090', '\u0098', '\u009e', '\u009f':
				s.state = sanitizeString
			default:
				if r == '\n' || r == '\t' || !unicode.IsControl(r) {
					cleaned.WriteRune(r)
				}
			}
		case sanitizeEscape:
			switch r {
			case '[':
				s.state = sanitizeCSI
			case ']':
				s.state = sanitizeOSC
			case 'P', 'X', '^', '_':
				s.state = sanitizeString
			default:
				if r >= 0x20 && r <= 0x2f {
					continue
				}
				s.state = sanitizeText
				if r == '\n' || r == '\t' {
					cleaned.WriteRune(r)
				}
			}
		case sanitizeCSI:
			// CSI ends with a final byte in 0x40..0x7e. Invalid controls also
			// terminate it, but are never rendered.
			if (r >= 0x40 && r <= 0x7e) || unicode.IsControl(r) {
				s.state = sanitizeText
			}
		case sanitizeOSC:
			s.consumeString(r, true)
		case sanitizeString:
			s.consumeString(r, false)
		}
	}
	return cleaned.String()
}

func (s *terminalSanitizer) consumeString(r rune, bellTerminates bool) {
	if bellTerminates && r == '\a' {
		s.state = sanitizeText
		s.stringEscape = false
		return
	}
	if r == '\u009c' {
		s.state = sanitizeText
		s.stringEscape = false
		return
	}
	if s.stringEscape {
		if r == '\\' {
			s.state = sanitizeText
		}
		s.stringEscape = false
		return
	}
	s.stringEscape = r == '\x1b'
}
