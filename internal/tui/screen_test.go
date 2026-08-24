package tui

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestRenderUnchangedFrameWritesNothing(t *testing.T) {
	var output bytes.Buffer
	screen := newScreen(&output, 80, 24)
	frame := Frame{
		Rows:          []string{"heading", "first row", "second row"},
		CursorRow:     2,
		CursorCol:     4,
		CursorVisible: true,
	}

	if err := screen.Render(frame); err != nil {
		t.Fatalf("first Render: %v", err)
	}
	output.Reset()
	writes := screen.stats.WriteCalls
	bytesWritten := screen.stats.BytesWritten

	if err := screen.Render(frame); err != nil {
		t.Fatalf("unchanged Render: %v", err)
	}
	if got := output.Len(); got != 0 {
		t.Fatalf("unchanged frame emitted %d bytes: %q", got, output.String())
	}
	if screen.stats.WriteCalls != writes || screen.stats.BytesWritten != bytesWritten {
		t.Fatalf("unchanged frame changed write stats: before=(%d,%d), after=(%d,%d)",
			writes, bytesWritten, screen.stats.WriteCalls, screen.stats.BytesWritten)
	}
}

func TestRenderOneChangedRowDoesNotRedrawAll(t *testing.T) {
	var output bytes.Buffer
	screen := newScreen(&output, 80, 24)
	rows := make([]string, 20)
	for i := range rows {
		rows[i] = strings.Repeat(string(rune('a'+i)), 12)
	}
	frame := Frame{Rows: rows, CursorRow: 19, CursorVisible: true}
	if err := screen.Render(frame); err != nil {
		t.Fatalf("first Render: %v", err)
	}

	output.Reset()
	beforeRows := screen.stats.RowsRendered
	beforeWrites := screen.stats.WriteCalls
	changed := append([]string(nil), rows...)
	changed[7] = "the only changed row"
	if err := screen.Render(Frame{Rows: changed, CursorRow: 19, CursorVisible: true}); err != nil {
		t.Fatalf("changed Render: %v", err)
	}

	got := output.String()
	if delta := screen.stats.RowsRendered - beforeRows; delta != 1 {
		t.Fatalf("rendered %d rows, want 1; output %q", delta, got)
	}
	if delta := screen.stats.WriteCalls - beforeWrites; delta != 1 {
		t.Fatalf("made %d writes, want one buffered write", delta)
	}
	if count := strings.Count(got, "\x1b[2K"); count != 1 {
		t.Fatalf("got %d erase-line controls, want 1; output %q", count, got)
	}
	if !strings.Contains(got, "\x1b[8;1H\x1b[2Kthe only changed row\x1b[0m") {
		t.Fatalf("changed row was not positioned, erased, and reset: %q", got)
	}
	if strings.Contains(got, rows[6]) || strings.Contains(got, rows[8]) {
		t.Fatalf("adjacent unchanged rows were redrawn: %q", got)
	}
}

func TestRenderFrameHeightChangeClearsRemovedRows(t *testing.T) {
	var output bytes.Buffer
	screen := newScreen(&output, 20, 10)
	if err := screen.Render(Frame{Rows: []string{"one", "two", "three"}}); err != nil {
		t.Fatal(err)
	}
	output.Reset()

	if err := screen.Render(Frame{Rows: []string{"one"}}); err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(output.String(), "\x1b[2K"); count != 3 {
		t.Fatalf("height change erased %d rows, want all 3 old/new rows; output %q", count, output.String())
	}
}

func TestRenderClampsRowsAndCursor(t *testing.T) {
	var output bytes.Buffer
	screen := newScreen(&output, 4, 2)
	frame := Frame{
		Rows:          []string{"abcdef", "ok", "not visible"},
		CursorRow:     99,
		CursorCol:     -5,
		CursorVisible: true,
	}
	if err := screen.Render(frame); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.Contains(got, "e") || strings.Contains(got, "not visible") {
		t.Fatalf("rows were not clamped to screen: %q", got)
	}
	if !strings.Contains(got, "\x1b[2;1H\x1b[?25h") {
		t.Fatalf("cursor was not clamped to row 2, column 1: %q", got)
	}
}

func TestOpenScreenRejectsNonTTYWithoutWriting(t *testing.T) {
	in, inWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer inWriter.Close()
	outReader, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outReader.Close()
	defer out.Close()

	if screen, err := OpenScreen(in, out); err == nil || screen != nil {
		t.Fatalf("OpenScreen(non-TTY) = (%v, %v), want nil screen and error", screen, err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	written, err := io.ReadAll(outReader)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 0 {
		t.Fatalf("OpenScreen wrote terminal controls to non-TTY output: %q", written)
	}
}

func TestFrameSelectionHighlightsAndCopiesPlainText(t *testing.T) {
	frame := Frame{
		Rows:      []string{"\x1b[31mhello\x1b[0m world", "second row"},
		Selection: &Selection{Start: SelectionPoint{Row: 0, Col: 3}, End: SelectionPoint{Row: 1, Col: 5}},
	}
	rows := append([]string(nil), frame.Rows...)
	applySelection(rows, *frame.Selection, 20)
	if !strings.Contains(rows[0], "\x1b[7m") || !strings.Contains(rows[1], "\x1b[7m") {
		t.Fatalf("selection was not highlighted: %#v", rows)
	}
	if got, want := selectedText(frame), "lo world\nsecond"; got != want {
		t.Fatalf("selectedText = %q, want %q", got, want)
	}
	frame.Selection = &Selection{Start: frame.Selection.End, End: frame.Selection.Start}
	if got, want := selectedText(frame), "lo world\nsecond"; got != want {
		t.Fatalf("reverse selectedText = %q, want %q", got, want)
	}
}

func TestFrameSelectionHandlesWideAndSpaceCells(t *testing.T) {
	wide := Frame{Rows: []string{"界x"}, Selection: &Selection{Start: SelectionPoint{Row: 0, Col: 1}, End: SelectionPoint{Row: 0, Col: 1}}}
	if got := selectedText(wide); got != "界" {
		t.Fatalf("wide continuation selection = %q", got)
	}
	rows := append([]string(nil), wide.Rows...)
	applySelection(rows, *wide.Selection, 3)
	if !strings.HasPrefix(rows[0], "\x1b[7m界\x1b[27m") {
		t.Fatalf("wide highlight = %q", rows[0])
	}
	combining := Frame{Rows: []string{"e\u0301x"}, Selection: &Selection{Start: SelectionPoint{}, End: SelectionPoint{}}}
	if got := selectedText(combining); got != "e\u0301" {
		t.Fatalf("combining selection = %q", got)
	}
	spaces := Frame{Rows: []string{"a  b     ", "   "}, Selection: &Selection{Start: SelectionPoint{Row: 0, Col: 1}, End: SelectionPoint{Row: 1, Col: 2}}}
	if got, want := selectedText(spaces), "  b\n   "; got != want {
		t.Fatalf("space selection = %q, want %q", got, want)
	}
}

func TestClampRowUsesDisplayCellWidths(t *testing.T) {
	if got := clampRow("界界x", 3); got != "界x" {
		t.Fatalf("clampRow = %q", got)
	}
}

func TestTerminalModesIncludeMouseTracking(t *testing.T) {
	setup := terminalSetupSequence(enableModifyOtherKeys, true)
	if !strings.Contains(setup, "\x1b[?1002h") || !strings.Contains(setup, "\x1b[?1006h") {
		t.Fatalf("setup does not enable mouse modes: %q", setup)
	}
	if disabled := terminalSetupSequence(enableModifyOtherKeys, false); strings.Contains(disabled, "?1002h") || strings.Contains(disabled, "?1006h") {
		t.Fatalf("disabled setup enables mouse modes: %q", disabled)
	}
	cleanup := terminalCleanupSequence(true, enableModifyOtherKeys)
	if !strings.Contains(cleanup, "\x1b[?1006l") || !strings.Contains(cleanup, "\x1b[?1002l") {
		t.Fatalf("cleanup does not disable mouse modes: %q", cleanup)
	}
}

func TestEnhancedKeyboardSetup(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	if got := enhancedKeyboardSetup(env(map[string]string{"TMUX": "/tmp/tmux", "TERM": "xterm-ghostty"})); got != enableModifyOtherKeys {
		t.Fatalf("tmux setup = %q", got)
	}
	if got := enhancedKeyboardSetup(env(map[string]string{"TERM_PROGRAM": "ghostty"})); got != enableKittyKeyboard {
		t.Fatalf("Ghostty setup = %q", got)
	}
	if got := enhancedKeyboardSetup(env(map[string]string{"TERM": "xterm-256color"})); got != enableModifyOtherKeys {
		t.Fatalf("xterm setup = %q", got)
	}
}

func TestCleanupOnlyRestoresEnabledModes(t *testing.T) {
	cleanup := terminalCleanupSequence(false, enableKittyKeyboard)
	if strings.Contains(cleanup, "?1002l") || strings.Contains(cleanup, disableModifyOtherKeys) || !strings.Contains(cleanup, disableKittyKeyboard) {
		t.Fatalf("mode-specific cleanup = %q", cleanup)
	}
}

func TestCloseEmitsAllRestorationControlsOnce(t *testing.T) {
	var output bytes.Buffer
	screen := newScreen(&output, 80, 24)
	if err := screen.Close(); err != nil {
		t.Fatal(err)
	}
	want := terminalCleanupSequence(false, "")
	if got := output.String(); got != want {
		t.Fatalf("Close output = %q, want %q", got, want)
	}
	if err := screen.Close(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != want {
		t.Fatalf("second Close emitted more output: %q", got)
	}
}

func TestRenderWriteFailureInvalidatesCache(t *testing.T) {
	writer := &failOnceWriter{}
	screen := newScreen(writer, 20, 5)
	frame := Frame{Rows: []string{"one", "two"}}
	if err := screen.Render(frame); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Render error = %v, want %v", err, io.ErrUnexpectedEOF)
	}
	if screen.valid {
		t.Fatal("screen cache remained valid after a partial write")
	}
	if err := screen.Render(frame); err != nil {
		t.Fatalf("retry Render: %v", err)
	}
	if got := screen.stats.RowsRendered; got != 4 {
		t.Fatalf("rows rendered across failure and retry = %d, want 4", got)
	}
}

type failOnceWriter struct {
	failed bool
	bytes.Buffer
}

func (w *failOnceWriter) Write(p []byte) (int, error) {
	if !w.failed {
		w.failed = true
		n := len(p) / 2
		_, _ = w.Buffer.Write(p[:n])
		return n, io.ErrUnexpectedEOF
	}
	return w.Buffer.Write(p)
}

func BenchmarkRenderUnchangedFrame(b *testing.B) {
	screen := newScreen(io.Discard, 120, 40)
	frame := Frame{Rows: make([]string, 40), CursorVisible: true}
	for i := range frame.Rows {
		frame.Rows[i] = strings.Repeat("x", 100)
	}
	if err := screen.Render(frame); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := screen.Render(frame); err != nil {
			b.Fatal(err)
		}
	}
}
