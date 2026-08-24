// Package tui provides the low-level, event-driven terminal screen used by the
// interactive user interface.
package tui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

const (
	enterAlternateScreen   = "\x1b[?1049h"
	leaveAlternateScreen   = "\x1b[?1049l"
	enableBracketedPaste   = "\x1b[?2004h"
	disableBracketedPaste  = "\x1b[?2004l"
	enableKittyKeyboard    = "\x1b[>7u" // disambiguation, event types, and alternate keys
	disableKittyKeyboard   = "\x1b[<u"  // pop the keyboard mode pushed above
	enableModifyOtherKeys  = "\x1b[>4;2m"
	disableModifyOtherKeys = "\x1b[>4;0m"
	enableMouseTracking    = "\x1b[?1002h\x1b[?1006h"
	disableMouseTracking   = "\x1b[?1006l\x1b[?1002l"
	showCursor             = "\x1b[?25h"
	hideCursor             = "\x1b[?25l"
	resetSGR               = "\x1b[0m"
)

var errScreenClosed = errors.New("tui: screen is closed")

type SelectionPoint struct {
	Row, Col int
}

type Selection struct {
	Start, End SelectionPoint
}

// Frame is a complete desired screen frame. CursorRow and CursorCol are
// zero-based terminal coordinates.
type Frame struct {
	Rows          []string
	CursorRow     int
	CursorCol     int
	CursorVisible bool
	Selection     *Selection
}

// screenStats is intentionally kept on Screen so package tests and benchmarks
// can distinguish diffing work from writes without instrumenting a writer.
type screenStats struct {
	RenderCalls  uint64
	RowsRendered uint64
	WriteCalls   uint64
	BytesWritten uint64
}

// Screen owns terminal mode and a cache of the last successfully rendered
// frame. It has no polling loop: size checks and output happen only when a
// caller invokes Size or Render.
type Screen struct {
	mu sync.Mutex

	in           *os.File
	out          io.Writer
	oldState     *term.State
	sizeFn       func() (width, height int, err error)
	closed       bool
	mouseEnabled bool
	keyboardMode string

	valid          bool
	width          int
	height         int
	previousRows   []string
	previousHeight int
	cursorKnown    bool
	cursorRow      int
	cursorCol      int
	cursorVisible  bool
	stats          screenStats
}

// OpenScreen puts in into raw mode and enables the output terminal's alternate
// screen, bracketed-paste mode, enhanced keyboard reporting, and mouse capture.
func OpenScreen(in *os.File, out *os.File) (*Screen, error) {
	return openScreen(in, out, true)
}

func OpenScreenWithoutMouse(in *os.File, out *os.File) (*Screen, error) {
	return openScreen(in, out, false)
}

func openScreen(in *os.File, out *os.File, mouse bool) (*Screen, error) {
	if in == nil || out == nil {
		return nil, errors.New("tui: input and output terminals are required")
	}

	inFD, outFD := int(in.Fd()), int(out.Fd())
	// Check everything that can be checked before changing terminal state. In
	// particular, never emit terminal escapes to redirected output.
	if !term.IsTerminal(inFD) {
		return nil, errors.New("tui: input is not a terminal")
	}
	if !term.IsTerminal(outFD) {
		return nil, errors.New("tui: output is not a terminal")
	}
	if width, height, err := term.GetSize(outFD); err != nil {
		return nil, fmt.Errorf("tui: get terminal size: %w", err)
	} else if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("tui: invalid terminal size %dx%d", width, height)
	}

	oldState, err := term.MakeRaw(inFD)
	if err != nil {
		return nil, fmt.Errorf("tui: enter raw mode: %w", err)
	}

	keyboardMode := enhancedKeyboardSetup(os.Getenv)
	setup := terminalSetupSequence(keyboardMode, mouse)
	if err := writeOnce(out, []byte(setup)); err != nil {
		// Best-effort all terminal cleanup, followed by an unconditional raw-mode
		// restore. The original setup error remains the primary error.
		cleanupErr := writeOnce(out, []byte(terminalCleanupSequence(mouse, keyboardMode)))
		restoreErr := term.Restore(inFD, oldState)
		return nil, errors.Join(fmt.Errorf("tui: initialize terminal: %w", err), cleanupErr, restoreErr)
	}

	return &Screen{
		in:           in,
		out:          out,
		oldState:     oldState,
		mouseEnabled: mouse,
		keyboardMode: keyboardMode,
		sizeFn: func() (int, int, error) {
			return term.GetSize(outFD)
		},
	}, nil
}

// newScreen constructs a screen without touching a terminal. It is for pure
// package tests and benchmarks; production callers should use OpenScreen.
func newScreen(out io.Writer, width, height int) *Screen {
	return &Screen{
		out: out,
		sizeFn: func() (int, int, error) {
			return width, height, nil
		},
	}
}

// Size returns the terminal width and height in cells.
func (s *Screen) Size() (width, height int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, 0, errScreenClosed
	}
	return s.sizeLocked()
}

func (s *Screen) sizeLocked() (int, int, error) {
	if s.sizeFn == nil {
		return 0, 0, errors.New("tui: terminal size is unavailable")
	}
	width, height, err := s.sizeFn()
	if err != nil {
		return 0, 0, fmt.Errorf("tui: get terminal size: %w", err)
	}
	if width <= 0 || height <= 0 {
		return 0, 0, fmt.Errorf("tui: invalid terminal size %dx%d", width, height)
	}
	return width, height, nil
}

// Render writes only rows which differ from the previous successfully written
// frame. Output for one Render call is assembled first and sent with at most
// one call to the underlying writer.
func (s *Screen) Render(frame Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stats.RenderCalls++
	if s.closed {
		return errScreenClosed
	}
	width, height, err := s.sizeLocked()
	if err != nil {
		return err
	}

	rowCount := len(frame.Rows)
	if rowCount > height {
		rowCount = height
	}
	rows := make([]string, rowCount)
	for i := range rows {
		rows[i] = clampRow(frame.Rows[i], width)
	}
	if frame.Selection != nil {
		applySelection(rows, *frame.Selection, width)
	}

	invalidate := !s.valid || width != s.width || height != s.height || len(frame.Rows) != s.previousHeight
	lastRow := len(rows)
	if invalidate && len(s.previousRows) > lastRow {
		lastRow = len(s.previousRows)
	}
	if lastRow > height {
		lastRow = height
	}

	var buf bytes.Buffer
	rowsRendered := 0
	for row := 0; row < lastRow; row++ {
		var text string
		if row < len(rows) {
			text = rows[row]
		}
		changed := invalidate || row >= len(s.previousRows) || text != s.previousRows[row]
		if !changed {
			continue
		}
		writeCursorPosition(&buf, row, 0)
		buf.WriteString("\x1b[2K")
		buf.WriteString(text)
		// A row may set colors or attributes. Never allow those attributes to
		// leak into a later row or the user's restored terminal.
		buf.WriteString(resetSGR)
		rowsRendered++
	}

	cursorRow := clamp(frame.CursorRow, 0, height-1)
	cursorCol := clamp(frame.CursorCol, 0, width-1)
	// Drawing any row moves the physical cursor, so restore it even when the
	// requested cursor did not change. An entirely unchanged frame emits no
	// bytes once cursor state is known.
	if rowsRendered != 0 || !s.cursorKnown || cursorRow != s.cursorRow || cursorCol != s.cursorCol {
		writeCursorPosition(&buf, cursorRow, cursorCol)
	}
	if !s.cursorKnown || frame.CursorVisible != s.cursorVisible {
		if frame.CursorVisible {
			buf.WriteString(showCursor)
		} else {
			buf.WriteString(hideCursor)
		}
	}

	s.stats.RowsRendered += uint64(rowsRendered)
	if buf.Len() != 0 {
		s.stats.WriteCalls++
		n, writeErr := s.out.Write(buf.Bytes())
		if n > 0 {
			s.stats.BytesWritten += uint64(n)
		}
		if writeErr == nil && n != buf.Len() {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			// The terminal may have accepted an arbitrary prefix. Do not compare
			// future frames against a state which may not exist on screen.
			s.valid = false
			s.cursorKnown = false
			return fmt.Errorf("tui: render: %w", writeErr)
		}
	}

	s.valid = true
	s.width, s.height = width, height
	s.previousRows = rows
	s.previousHeight = len(frame.Rows)
	s.cursorKnown = true
	s.cursorRow, s.cursorCol = cursorRow, cursorCol
	s.cursorVisible = frame.CursorVisible
	return nil
}

// Close restores terminal presentation modes and then the original input mode.
// Restoration of the input state is attempted even if writing terminal escapes
// fails. Close is idempotent.
func (s *Screen) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	var outputErr, restoreErr error
	if s.out != nil {
		if err := writeOnce(s.out, []byte(terminalCleanupSequence(s.mouseEnabled, s.keyboardMode))); err != nil {
			outputErr = fmt.Errorf("tui: restore output terminal: %w", err)
		}
	}
	if s.in != nil && s.oldState != nil {
		if err := term.Restore(int(s.in.Fd()), s.oldState); err != nil {
			restoreErr = fmt.Errorf("tui: restore input terminal: %w", err)
		}
	}
	return errors.Join(outputErr, restoreErr)
}

func enhancedKeyboardSetup(getenv func(string) string) string {
	// tmux currently answers the Kitty protocol query with device attributes but
	// no Kitty flags in common configurations. Pi falls back to xterm's
	// modifyOtherKeys in that case; do the same directly so Shift+Enter survives
	// the tmux boundary without a startup timeout. Native Ghostty, Kitty, WezTerm,
	// and compatible terminals use Kitty's richer protocol.
	if getenv("TMUX") != "" {
		return enableModifyOtherKeys
	}
	program := strings.ToLower(getenv("TERM_PROGRAM"))
	termName := strings.ToLower(getenv("TERM"))
	if getenv("KITTY_WINDOW_ID") != "" || strings.Contains(program, "kitty") || strings.Contains(program, "ghostty") || strings.Contains(program, "wezterm") || strings.Contains(termName, "kitty") || strings.Contains(termName, "ghostty") {
		return enableKittyKeyboard
	}
	return enableModifyOtherKeys
}

func terminalSetupSequence(keyboardMode string, mouse bool) string {
	setup := enterAlternateScreen + enableBracketedPaste
	if mouse {
		setup += enableMouseTracking
	}
	return setup + keyboardMode
}

func terminalCleanupSequence(mouse bool, keyboardMode string) string {
	cleanup := resetSGR + showCursor
	if mouse {
		cleanup += disableMouseTracking
	}
	if keyboardMode == enableKittyKeyboard {
		cleanup += disableKittyKeyboard
	}
	if keyboardMode == enableModifyOtherKeys {
		cleanup += disableModifyOtherKeys
	}
	return cleanup + disableBracketedPaste + leaveAlternateScreen
}

func writeOnce(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err == nil && n != len(p) {
		return io.ErrShortWrite
	}
	return err
}

func writeCursorPosition(buf *bytes.Buffer, row, col int) {
	fmt.Fprintf(buf, "\x1b[%d;%dH", row+1, col+1)
}

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func normalizeSelection(selection Selection) (SelectionPoint, SelectionPoint) {
	start, end := selection.Start, selection.End
	if start.Row > end.Row || (start.Row == end.Row && start.Col > end.Col) {
		start, end = end, start
	}
	return start, end
}

func applySelection(rows []string, selection Selection, width int) {
	if len(rows) == 0 || width <= 0 {
		return
	}
	start, end := normalizeSelection(selection)
	start.Row = clamp(start.Row, 0, max(0, len(rows)-1))
	end.Row = clamp(end.Row, 0, max(0, len(rows)-1))
	start.Col = clamp(start.Col, 0, max(0, width-1))
	end.Col = clamp(end.Col, 0, max(0, width-1))
	for row := start.Row; row <= end.Row; row++ {
		from, to := 0, width-1
		if row == start.Row {
			from = start.Col
		}
		if row == end.Row {
			to = end.Col
		}
		rows[row] = highlightRow(rows[row], from, to)
	}
}

func highlightRow(row string, from, to int) string {
	if from > to {
		return row
	}
	var out strings.Builder
	cells := 0
	highlighting := false
	for i := 0; i < len(row); {
		if row[i] == '\x1b' {
			end, sgr := controlSequenceEnd(row, i)
			out.WriteString(row[i:end])
			if sgr && highlighting {
				out.WriteString("\x1b[7m")
			}
			i = end
			continue
		}
		r, size := utf8.DecodeRuneInString(row[i:])
		width := max(0, runewidth.RuneWidth(r))
		selected := width == 0 && highlighting || width > 0 && cells+width > from && cells <= to
		if selected && !highlighting {
			out.WriteString("\x1b[7m")
			highlighting = true
		}
		if !selected && highlighting {
			out.WriteString("\x1b[27m")
			highlighting = false
		}
		out.WriteString(row[i : i+size])
		i += size
		cells += width
	}
	if highlighting {
		out.WriteString("\x1b[27m")
	}
	return out.String()
}

func selectedText(frame Frame) string {
	if frame.Selection == nil {
		return ""
	}
	start, end := normalizeSelection(*frame.Selection)
	if len(frame.Rows) == 0 {
		return ""
	}
	start.Row = clamp(start.Row, 0, len(frame.Rows)-1)
	end.Row = clamp(end.Row, 0, len(frame.Rows)-1)
	var lines []string
	for row := start.Row; row <= end.Row; row++ {
		plain := plainTerminalRow(frame.Rows[row])
		from, to := 0, max(0, visibleWidth(plain)-1)
		if row == start.Row {
			from = start.Col
		}
		if row == end.Row {
			to = end.Col
		}
		line := sliceCells(plain, from, to+1)
		if row != end.Row {
			line = strings.TrimRight(line, " ")
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func plainTerminalRow(row string) string {
	var out strings.Builder
	for i := 0; i < len(row); {
		if row[i] == '\x1b' {
			end, _ := controlSequenceEnd(row, i)
			i = end
			continue
		}
		r, size := utf8.DecodeRuneInString(row[i:])
		i += size
		if !unicode.IsControl(r) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func sliceCells(text string, from, to int) string {
	if from >= to {
		return ""
	}
	var out strings.Builder
	cells := 0
	selectedBase := false
	for _, r := range text {
		w := max(0, runewidth.RuneWidth(r))
		if w == 0 {
			if selectedBase {
				out.WriteRune(r)
			}
			continue
		}
		if cells >= to {
			break
		}
		selectedBase = cells+w > from && cells < to
		if selectedBase {
			out.WriteRune(r)
		}
		cells += w
	}
	return out.String()
}

// clampRow removes terminal controls other than SGR and limits ordinary runes
// to the available cells. Keeping only SGR prevents embedded rows from moving
// the cursor and invalidating Screen's cache. Combining marks do not consume a
// cell; other runes conservatively consume one cell.
func clampRow(row string, width int) string {
	if width <= 0 || row == "" {
		return ""
	}
	var out strings.Builder
	cells := 0
	for i := 0; i < len(row); {
		if row[i] == '\x1b' {
			end, sgr := controlSequenceEnd(row, i)
			if sgr && cells < width {
				out.WriteString(row[i:end])
			}
			i = end
			continue
		}

		r, size := utf8.DecodeRuneInString(row[i:])
		if r == utf8.RuneError && size == 1 {
			r, size = utf8.RuneError, 1
		}
		i += size
		if unicode.IsControl(r) {
			continue
		}
		runeWidth := max(0, runewidth.RuneWidth(r))
		if runeWidth == 0 {
			if cells != 0 {
				out.WriteRune(r)
			}
			continue
		}
		if cells+runeWidth > width {
			continue
		}
		out.WriteRune(r)
		cells += runeWidth
	}
	return out.String()
}

// controlSequenceEnd consumes an ESC sequence and reports whether it is a CSI
// Select Graphic Rendition sequence. Unknown and unterminated controls are
// discarded rather than copied to the terminal.
func controlSequenceEnd(s string, start int) (end int, sgr bool) {
	if start+1 >= len(s) {
		return len(s), false
	}
	if s[start+1] != '[' {
		// Consume a two-byte escape sequence. For OSC, also consume its payload
		// through BEL or ST so it cannot become visible text.
		if s[start+1] == ']' {
			for i := start + 2; i < len(s); i++ {
				if s[i] == '\a' {
					return i + 1, false
				}
				if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '\\' {
					return i + 2, false
				}
			}
			return len(s), false
		}
		return start + 2, false
	}
	for i := start + 2; i < len(s); i++ {
		if s[i] >= 0x40 && s[i] <= 0x7e {
			return i + 1, s[i] == 'm'
		}
	}
	return len(s), false
}
