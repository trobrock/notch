package tui

import (
	"reflect"
	"testing"
)

func TestParserKeys(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []KeyEvent
	}{
		{"text", "a界", []KeyEvent{{Text: "a"}, {Text: "界"}}},
		{"enter", "\r", []KeyEvent{{Key: KeyEnter}}},
		{"ctrl-j", "\n", []KeyEvent{{Key: KeyNewline}}},
		{"backspace-del", "\x7f", []KeyEvent{{Key: KeyBackspace}}},
		{"backspace-bs", "\x08", []KeyEvent{{Key: KeyBackspace}}},
		{"tab", "\t", []KeyEvent{{Key: KeyTab}}},
		{"controls", "\x01\x05\x02\x06\x0b\x15\x17\x03\x04", []KeyEvent{
			{Key: KeyCtrlA}, {Key: KeyCtrlE}, {Key: KeyCtrlB}, {Key: KeyCtrlF},
			{Key: KeyCtrlK}, {Key: KeyCtrlU}, {Key: KeyCtrlW}, {Key: KeyCtrlC}, {Key: KeyCtrlD},
		}},
		{"arrows", "\x1b[A\x1b[B\x1b[C\x1b[D", []KeyEvent{{Key: KeyUp}, {Key: KeyDown}, {Key: KeyRight}, {Key: KeyLeft}}},
		{"ss3-arrows", "\x1bOA\x1bOB\x1bOC\x1bOD", []KeyEvent{{Key: KeyUp}, {Key: KeyDown}, {Key: KeyRight}, {Key: KeyLeft}}},
		{"navigation", "\x1b[H\x1b[F\x1b[3~\x1b[5~\x1b[6~", []KeyEvent{
			{Key: KeyHome}, {Key: KeyEnd}, {Key: KeyDelete}, {Key: KeyPageUp}, {Key: KeyPageDown},
		}},
		{"tilde-home-end", "\x1b[1~\x1b[4~\x1b[7~\x1b[8~", []KeyEvent{{Key: KeyHome}, {Key: KeyEnd}, {Key: KeyHome}, {Key: KeyEnd}}},
		{"alt-emacs", "\x1bb\x1bf", []KeyEvent{{Key: KeyAltLeft}, {Key: KeyAltRight}}},
		{"alt-xterm", "\x1b[1;3D\x1b[1;3C", []KeyEvent{{Key: KeyAltLeft}, {Key: KeyAltRight}}},
		{"alt-rxvt", "\x1b[3D\x1b[3C", []KeyEvent{{Key: KeyAltLeft}, {Key: KeyAltRight}}},
		{"shift-enter", "\x1b[13;2u\x1b[27;2;13~", []KeyEvent{{Key: KeyShiftEnter}, {Key: KeyShiftEnter}}},
		{"shift-tab", "\x1b[Z\x1b[9;2u\x1b[27;2;9~", []KeyEvent{{Key: KeyShiftTab}, {Key: KeyShiftTab}, {Key: KeyShiftTab}}},
		{"kitty-controls", "\x1b[100;5u\x1b[99;5u\x1b[106;5u\x1b[97;5:3u", []KeyEvent{{Key: KeyCtrlD}, {Key: KeyCtrlC}, {Key: KeyNewline}}},
		{"modify-other-controls", "\x1b[27;5;100~\x1b[27;5;99~", []KeyEvent{{Key: KeyCtrlD}, {Key: KeyCtrlC}}},
		{"alt-enter", "\x1b[13;3u\x1b[27;3;13~\x1b\r", []KeyEvent{{Key: KeyAltEnter}, {Key: KeyAltEnter}, {Key: KeyAltEnter}}},
		{"kitty-query-response", "\x1b[?7u", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p Parser
			if got := p.Feed([]byte(tt.in)); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Feed(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParserRetainsEverySplitSequence(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []KeyEvent
	}{
		{"utf8", "x界🙂y", []KeyEvent{{Text: "x"}, {Text: "界"}, {Text: "🙂"}, {Text: "y"}}},
		{"escape", "\x1b[1;3D", []KeyEvent{{Key: KeyAltLeft}}},
		{"shift-enter", "\x1b[27;2;13~", []KeyEvent{{Key: KeyShiftEnter}}},
		{"shift-tab", "\x1b[9;2u", []KeyEvent{{Key: KeyShiftTab}}},
		{"kitty-ctrl-d", "\x1b[100;5u", []KeyEvent{{Key: KeyCtrlD}}},
		{"kitty-alt-enter", "\x1b[13;3u", []KeyEvent{{Key: KeyAltEnter}}},
		{"paste", "\x1b[200~first\n二\r\n🙂last\x1b[201~", []KeyEvent{{Text: "first\n二\r\n🙂last", Paste: true}}},
		{"paste-and-surrounding-text", "a\x1b[200~b\nc\x1b[201~d", []KeyEvent{{Text: "a"}, {Text: "b\nc", Paste: true}, {Text: "d"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for split := 0; split <= len(tt.in); split++ {
				var p Parser
				got := p.Feed([]byte(tt.in[:split]))
				got = append(got, p.Feed([]byte(tt.in[split:]))...)
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("split %d: %#v, want %#v", split, got, tt.want)
				}
			}
		})
	}
}

func TestParserPasteWaitsForTerminator(t *testing.T) {
	var p Parser
	if got := p.Feed([]byte("\x1b[200~hello\n")); len(got) != 0 {
		t.Fatalf("unterminated paste emitted %#v", got)
	}
	if got := p.Feed([]byte("world\x1b[20")); len(got) != 0 {
		t.Fatalf("split terminator emitted %#v", got)
	}
	want := []KeyEvent{{Text: "hello\nworld", Paste: true}, {Text: "!"}}
	if got := p.Feed([]byte("1~!")); !reflect.DeepEqual(got, want) {
		t.Fatalf("completed paste = %#v, want %#v", got, want)
	}
}

func TestParserFlushEscape(t *testing.T) {
	var p Parser
	if got := p.Feed([]byte("\x1b")); got != nil {
		t.Fatalf("bare escape was not retained: %#v", got)
	}
	if got := p.FlushEscape(); !reflect.DeepEqual(got, []KeyEvent{{Key: KeyEscape}}) {
		t.Fatalf("FlushEscape = %#v", got)
	}

	if got := p.Feed([]byte("\x1b[")); got != nil {
		t.Fatalf("partial CSI was not retained: %#v", got)
	}
	want := []KeyEvent{{Key: KeyEscape}, {Text: "["}}
	if got := p.FlushEscape(); !reflect.DeepEqual(got, want) {
		t.Fatalf("partial CSI flush = %#v, want %#v", got, want)
	}
}
