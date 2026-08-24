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

func TestCloseEmitsAllRestorationControlsOnce(t *testing.T) {
	var output bytes.Buffer
	screen := newScreen(&output, 80, 24)
	if err := screen.Close(); err != nil {
		t.Fatal(err)
	}
	want := terminalCleanupSequence()
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
