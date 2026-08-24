// Package tui contains the terminal input parser and line editor used by the
// interactive user interface.
package tui

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Key names emitted by Parser. Printable input is represented by Text instead.
const (
	KeyEscape     = "escape"
	KeyEnter      = "enter"
	KeyNewline    = "newline"
	KeyBackspace  = "backspace"
	KeyTab        = "tab"
	KeyShiftTab   = "shift+tab"
	KeyUp         = "up"
	KeyDown       = "down"
	KeyLeft       = "left"
	KeyRight      = "right"
	KeyHome       = "home"
	KeyEnd        = "end"
	KeyDelete     = "delete"
	KeyPageUp     = "pageup"
	KeyPageDown   = "pagedown"
	KeyAltLeft    = "alt+left"
	KeyAltRight   = "alt+right"
	KeyAltEnter   = "alt+enter"
	KeyScrollUp   = "scroll+up"
	KeyScrollDown = "scroll+down"
	// Ctrl-J and Shift-Enter both request insertion of a newline; terminals
	// merely use different byte encodings for those equivalent actions.
	KeyShiftEnter = KeyNewline
	KeyCtrlA      = "ctrl+a"
	KeyCtrlB      = "ctrl+b"
	KeyCtrlC      = "ctrl+c"
	KeyCtrlD      = "ctrl+d"
	KeyCtrlE      = "ctrl+e"
	KeyCtrlF      = "ctrl+f"
	KeyCtrlK      = "ctrl+k"
	KeyCtrlU      = "ctrl+u"
	KeyCtrlW      = "ctrl+w"
	KeyCtrlY      = "ctrl+y"
)

type MouseAction string

const (
	MousePress   MouseAction = "press"
	MouseDrag    MouseAction = "drag"
	MouseRelease MouseAction = "release"
)

type MouseEvent struct {
	Action           MouseAction
	Button           int
	Row, Col         int
	Shift, Alt, Ctrl bool
}

// KeyEvent is either a named terminal key, mouse event, or text. Paste is true only for the
// single text event produced by a bracketed paste.
type KeyEvent struct {
	Key   string
	Text  string
	Paste bool
	Mouse *MouseEvent
}

// Parser incrementally decodes bytes read from a terminal. Its zero value is
// ready for use. Incomplete UTF-8 and escape sequences remain buffered until a
// later Feed call; callers need not use a polling timeout except to distinguish
// a literal Escape key, for which FlushEscape is provided.
type Parser struct {
	buf     []byte
	inPaste bool
}

func NewParser() *Parser { return &Parser{} }

var pasteEnd = []byte("\x1b[201~")

// Feed parses all complete events in b.
func (p *Parser) Feed(b []byte) []KeyEvent {
	p.buf = append(p.buf, b...)
	var events []KeyEvent

	for len(p.buf) != 0 {
		if p.inPaste {
			i := bytes.Index(p.buf, pasteEnd)
			if i < 0 {
				// Keep all paste bytes. In particular this retains a partial end
				// marker and a partial UTF-8 rune without any special polling.
				break
			}
			events = append(events, KeyEvent{Text: string(p.buf[:i]), Paste: true})
			p.buf = p.buf[i+len(pasteEnd):]
			p.inPaste = false
			continue
		}

		if p.buf[0] == 0x1b {
			event, consumed, complete, pasteStart := parseEscape(p.buf)
			if !complete {
				break
			}
			p.buf = p.buf[consumed:]
			if pasteStart {
				p.inPaste = true
				continue
			}
			if event.Key != "" || event.Text != "" || event.Mouse != nil {
				events = append(events, event)
			}
			continue
		}

		if event, ok := controlEvent(p.buf[0]); ok {
			p.buf = p.buf[1:]
			events = append(events, event)
			continue
		}

		if !utf8.FullRune(p.buf) {
			break
		}
		r, n := utf8.DecodeRune(p.buf)
		p.buf = p.buf[n:]
		events = append(events, KeyEvent{Text: string(r)})
	}
	return events
}

func (p *Parser) HasPendingEscape() bool {
	return !p.inPaste && len(p.buf) > 0 && p.buf[0] == 0x1b
}

// FlushEscape resolves a pending escape prefix as a literal Escape key. It is
// normally called only after a terminal read deadline. Any bytes following the
// escape are fed back through the parser as ordinary input.
func (p *Parser) FlushEscape() []KeyEvent {
	if p.inPaste || len(p.buf) == 0 || p.buf[0] != 0x1b {
		return nil
	}
	rest := append([]byte(nil), p.buf[1:]...)
	p.buf = nil
	events := []KeyEvent{{Key: KeyEscape}}
	if len(rest) != 0 {
		events = append(events, p.Feed(rest)...)
	}
	return events
}

func controlEvent(b byte) (KeyEvent, bool) {
	key := ""
	switch b {
	case '\r':
		key = KeyEnter
	case '\n':
		key = KeyNewline
	case '\t':
		key = KeyTab
	case 0x7f, 0x08:
		key = KeyBackspace
	case 0x01:
		key = KeyCtrlA
	case 0x02:
		key = KeyCtrlB
	case 0x03:
		key = KeyCtrlC
	case 0x04:
		key = KeyCtrlD
	case 0x05:
		key = KeyCtrlE
	case 0x06:
		key = KeyCtrlF
	case 0x0b:
		key = KeyCtrlK
	case 0x15:
		key = KeyCtrlU
	case 0x17:
		key = KeyCtrlW
	case 0x19:
		key = KeyCtrlY
	default:
		return KeyEvent{}, false
	}
	return KeyEvent{Key: key}, true
}

// parseEscape returns (event, bytes consumed, complete, starts paste).
func parseEscape(b []byte) (KeyEvent, int, bool, bool) {
	if len(b) == 1 {
		return KeyEvent{}, 0, false, false
	}

	switch b[1] {
	case '\r', '\n':
		return KeyEvent{Key: KeyAltEnter}, 2, true, false
	case 'b':
		return KeyEvent{Key: KeyAltLeft}, 2, true, false
	case 'f':
		return KeyEvent{Key: KeyAltRight}, 2, true, false
	case 'O': // SS3 cursor mode
		if len(b) < 3 {
			return KeyEvent{}, 0, false, false
		}
		key := mapFinalKey(b[2])
		if key == "" {
			return KeyEvent{Key: KeyEscape}, 1, true, false
		}
		return KeyEvent{Key: key}, 3, true, false
	case '[':
		if event, consumed, complete, ok := parseMouseX10(b); ok {
			return event, consumed, complete, false
		}
		if event, consumed, complete, ok := parseMouseSGR(b); ok {
			return event, consumed, complete, false
		}
		// A CSI sequence ends at its first final byte (0x40 through 0x7e).
		end := -1
		for i := 2; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				end = i
				break
			}
		}
		if end < 0 {
			return KeyEvent{}, 0, false, false
		}
		seq := string(b[2 : end+1])
		consumed := end + 1
		switch seq {
		case "200~":
			return KeyEvent{}, consumed, true, true
		case "201~": // Stray paste end; consume it without inserting text.
			return KeyEvent{}, consumed, true, false
		case "A":
			return KeyEvent{Key: KeyUp}, consumed, true, false
		case "B":
			return KeyEvent{Key: KeyDown}, consumed, true, false
		case "C":
			return KeyEvent{Key: KeyRight}, consumed, true, false
		case "D":
			return KeyEvent{Key: KeyLeft}, consumed, true, false
		case "H", "1~", "7~":
			return KeyEvent{Key: KeyHome}, consumed, true, false
		case "F", "4~", "8~":
			return KeyEvent{Key: KeyEnd}, consumed, true, false
		case "3~":
			return KeyEvent{Key: KeyDelete}, consumed, true, false
		case "5~":
			return KeyEvent{Key: KeyPageUp}, consumed, true, false
		case "6~":
			return KeyEvent{Key: KeyPageDown}, consumed, true, false
		case "Z", "9;2u", "9;2~", "27;2;9~":
			return KeyEvent{Key: KeyShiftTab}, consumed, true, false
		case "13;2u", "13;2~", "27;2;13~":
			return KeyEvent{Key: KeyShiftEnter}, consumed, true, false
		}
		if event, handled := parseEnhancedKey(seq); handled {
			return event, consumed, true, false
		}

		// xterm, rxvt, and kitty all use variants of modifier 3 (or 9,
		// alt+shift) for alt-arrow. Treat other modified arrows as their
		// unmodified movement so they are never inserted as text.
		final := seq[len(seq)-1]
		params := seq[:len(seq)-1]
		if final == 'C' || final == 'D' {
			if hasAltModifier(params) {
				if final == 'C' {
					return KeyEvent{Key: KeyAltRight}, consumed, true, false
				}
				return KeyEvent{Key: KeyAltLeft}, consumed, true, false
			}
			return KeyEvent{Key: mapFinalKey(final)}, consumed, true, false
		}
		if key := mapFinalKey(final); key != "" && strings.Contains(params, ";") {
			return KeyEvent{Key: key}, consumed, true, false
		}
		// Do not discard an unrecognised escape sequence: emit Escape and
		// parse its remaining bytes normally.
		return KeyEvent{Key: KeyEscape}, 1, true, false
	default:
		// Escape followed by an ordinary byte is not one of the supported
		// alt encodings. Resolve only Escape and leave the byte for Feed.
		return KeyEvent{Key: KeyEscape}, 1, true, false
	}
}

func parseMouseX10(b []byte) (KeyEvent, int, bool, bool) {
	if len(b) < 3 || b[2] != 'M' {
		return KeyEvent{}, 0, false, false
	}
	if len(b) < 6 {
		return KeyEvent{}, 0, false, true
	}
	if b[3] < 32 || b[4] < 32 || b[5] < 32 {
		return KeyEvent{}, 0, false, false
	}
	button := int(b[3]) - 32
	row, col := int(b[5])-33, int(b[4])-33
	return mouseEvent(button, button&3 != 3, row, col, 6)
}

func parseMouseSGR(b []byte) (KeyEvent, int, bool, bool) {
	if len(b) < 3 || b[2] != '<' {
		return KeyEvent{}, 0, false, false
	}
	end := -1
	for i := 3; i < len(b); i++ {
		if b[i] == 'M' || b[i] == 'm' {
			end = i
			break
		}
		if (b[i] < '0' || b[i] > '9') && b[i] != ';' {
			return KeyEvent{}, 0, false, false
		}
	}
	if end < 0 {
		return KeyEvent{}, 0, false, true
	}
	parts := strings.Split(string(b[3:end]), ";")
	if len(parts) != 3 {
		return KeyEvent{}, end + 1, true, true
	}
	button, buttonErr := strconv.Atoi(parts[0])
	col, colErr := strconv.Atoi(parts[1])
	row, rowErr := strconv.Atoi(parts[2])
	if buttonErr != nil || colErr != nil || rowErr != nil || col < 1 || row < 1 {
		return KeyEvent{}, end + 1, true, true
	}
	return mouseEvent(button, b[end] == 'M', row-1, col-1, end+1)
}

func mouseEvent(button int, pressed bool, row, col, consumed int) (KeyEvent, int, bool, bool) {
	if button&64 != 0 {
		return mouseWheelEvent(button, pressed, consumed)
	}
	rawButton := button
	button, motion := button&3, button&32 != 0
	action := MousePress
	if !pressed || (motion && button == 3) {
		action = MouseRelease
	} else if motion {
		action = MouseDrag
	}
	return KeyEvent{Mouse: &MouseEvent{
		Action: action, Button: button, Row: row, Col: col,
		Shift: rawButton&4 != 0, Alt: rawButton&8 != 0, Ctrl: rawButton&16 != 0,
	}}, consumed, true, true
}

func mouseWheelEvent(button int, pressed bool, consumed int) (KeyEvent, int, bool, bool) {
	if !pressed {
		return KeyEvent{}, consumed, true, true
	}
	switch button & 0x43 {
	case 64:
		return KeyEvent{Key: KeyScrollUp}, consumed, true, true
	case 65:
		return KeyEvent{Key: KeyScrollDown}, consumed, true, true
	default:
		return KeyEvent{}, consumed, true, true
	}
}

func parseEnhancedKey(seq string) (KeyEvent, bool) {
	// Kitty keyboard protocol: CSI codepoint ; modifiers[:event] u.
	// Event 3 is key release and must be consumed without triggering an action.
	if strings.HasSuffix(seq, "u") {
		params := strings.TrimSuffix(seq, "u")
		if strings.HasPrefix(params, "?") {
			return KeyEvent{}, true
		} // protocol response
		parts := strings.Split(params, ";")
		code, err := strconv.Atoi(strings.Split(parts[0], ":")[0])
		if err != nil {
			return KeyEvent{}, false
		}
		modifier, eventType := 1, 1
		if len(parts) > 1 {
			modifierParts := strings.Split(parts[1], ":")
			if value, parseErr := strconv.Atoi(modifierParts[0]); parseErr == nil {
				modifier = value
			}
			if len(modifierParts) > 1 {
				if value, parseErr := strconv.Atoi(modifierParts[1]); parseErr == nil {
					eventType = value
				}
			}
		}
		if eventType == 3 {
			return KeyEvent{}, true
		}
		return enhancedCodeEvent(code, modifier), true
	}

	// xterm modifyOtherKeys: CSI 27 ; modifiers ; codepoint ~.
	if strings.HasSuffix(seq, "~") {
		parts := strings.Split(strings.TrimSuffix(seq, "~"), ";")
		if len(parts) == 3 && parts[0] == "27" {
			modifier, modErr := strconv.Atoi(parts[1])
			code, codeErr := strconv.Atoi(parts[2])
			if modErr == nil && codeErr == nil {
				return enhancedCodeEvent(code, modifier), true
			}
		}
	}
	return KeyEvent{}, false
}

func enhancedCodeEvent(code, modifier int) KeyEvent {
	bits := max(0, modifier-1)
	shift, alt, ctrl := bits&1 != 0, bits&2 != 0, bits&4 != 0
	if alt && (code == 13 || code == 10) {
		return KeyEvent{Key: KeyAltEnter}
	}
	if shift && code == 13 {
		return KeyEvent{Key: KeyShiftEnter}
	}
	if shift && code == 9 {
		return KeyEvent{Key: KeyShiftTab}
	}
	if !ctrl {
		return KeyEvent{}
	}
	switch code {
	case 'a', 'A':
		return KeyEvent{Key: KeyCtrlA}
	case 'b', 'B':
		return KeyEvent{Key: KeyCtrlB}
	case 'c', 'C':
		return KeyEvent{Key: KeyCtrlC}
	case 'd', 'D':
		return KeyEvent{Key: KeyCtrlD}
	case 'e', 'E':
		return KeyEvent{Key: KeyCtrlE}
	case 'f', 'F':
		return KeyEvent{Key: KeyCtrlF}
	case 'h', 'H':
		return KeyEvent{Key: KeyBackspace}
	case 'j', 'J':
		return KeyEvent{Key: KeyNewline}
	case 'i', 'I':
		return KeyEvent{Key: KeyTab}
	case 'k', 'K':
		return KeyEvent{Key: KeyCtrlK}
	case 'u', 'U':
		return KeyEvent{Key: KeyCtrlU}
	case 'w', 'W':
		return KeyEvent{Key: KeyCtrlW}
	case 'y', 'Y':
		return KeyEvent{Key: KeyCtrlY}
	}
	return KeyEvent{}
}

func mapFinalKey(final byte) string {
	switch final {
	case 'A':
		return KeyUp
	case 'B':
		return KeyDown
	case 'C':
		return KeyRight
	case 'D':
		return KeyLeft
	case 'H':
		return KeyHome
	case 'F':
		return KeyEnd
	default:
		return ""
	}
}

func hasAltModifier(params string) bool {
	parts := strings.Split(params, ";")
	for _, part := range parts[1:] {
		if part == "3" || part == "9" {
			return true
		}
	}
	// rxvt commonly reports alt-arrow as CSI 3 D/C.
	return params == "3"
}
