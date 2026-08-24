package tui

import (
	"unicode"

	"github.com/mattn/go-runewidth"
)

// Editor is a rune-indexed, multiline line editor. Cursor positions returned by
// Cursor are rune offsets, never byte offsets.
type Editor struct {
	text   []rune
	cursor int

	history      []string
	historyIndex int
	draft        string
	preferredCol int
}

// NewEditor constructs an editor. Passing an initial history is optional.
func NewEditor(initialHistory ...[]string) *Editor {
	e := &Editor{historyIndex: -1, preferredCol: -1}
	if len(initialHistory) != 0 {
		e.SetHistory(initialHistory[0])
	}
	return e
}

func (e *Editor) Text() string { return string(e.text) }
func (e *Editor) Cursor() int  { return e.cursor }

// SetText replaces the editable text and places the cursor at its end.
func (e *Editor) SetText(s string) {
	e.text = []rune(s)
	e.cursor = len(e.text)
	e.leaveVerticalMovement()
}

// SetCursor moves to a rune offset, clamping positions outside the text.
func (e *Editor) SetCursor(pos int) {
	e.cursor = clampEditor(pos, 0, len(e.text))
	e.leaveVerticalMovement()
}

func (e *Editor) Insert(s string) {
	if s == "" {
		return
	}
	r := []rune(s)
	next := make([]rune, 0, len(e.text)+len(r))
	next = append(next, e.text[:e.cursor]...)
	next = append(next, r...)
	next = append(next, e.text[e.cursor:]...)
	e.text = next
	e.cursor += len(r)
	e.leaveVerticalMovement()
}

func (e *Editor) Backspace() bool {
	if e.cursor == 0 {
		return false
	}
	copy(e.text[e.cursor-1:], e.text[e.cursor:])
	e.text = e.text[:len(e.text)-1]
	e.cursor--
	e.leaveVerticalMovement()
	return true
}

func (e *Editor) Delete() bool {
	if e.cursor == len(e.text) {
		return false
	}
	copy(e.text[e.cursor:], e.text[e.cursor+1:])
	e.text = e.text[:len(e.text)-1]
	e.leaveVerticalMovement()
	return true
}

func (e *Editor) MoveLeft() bool {
	if e.cursor == 0 {
		return false
	}
	e.cursor--
	e.leaveVerticalMovement()
	return true
}

func (e *Editor) MoveRight() bool {
	if e.cursor == len(e.text) {
		return false
	}
	e.cursor++
	e.leaveVerticalMovement()
	return true
}

func (e *Editor) MoveWordLeft() bool {
	start := e.cursor
	for e.cursor > 0 && !isWord(e.text[e.cursor-1]) {
		e.cursor--
	}
	for e.cursor > 0 && isWord(e.text[e.cursor-1]) {
		e.cursor--
	}
	e.leaveVerticalMovement()
	return e.cursor != start
}

func (e *Editor) MoveWordRight() bool {
	start := e.cursor
	for e.cursor < len(e.text) && !isWord(e.text[e.cursor]) {
		e.cursor++
	}
	for e.cursor < len(e.text) && isWord(e.text[e.cursor]) {
		e.cursor++
	}
	e.leaveVerticalMovement()
	return e.cursor != start
}

func (e *Editor) MoveWordBackward() bool { return e.MoveWordLeft() }
func (e *Editor) MoveWordForward() bool  { return e.MoveWordRight() }

// MoveHome and MoveEnd operate on the current logical line.
func (e *Editor) MoveHome() bool {
	start := e.cursor
	e.cursor = e.lineStart(e.cursor)
	e.leaveVerticalMovement()
	return e.cursor != start
}

func (e *Editor) MoveEnd() bool {
	start := e.cursor
	e.cursor = e.lineEnd(e.cursor)
	e.leaveVerticalMovement()
	return e.cursor != start
}

// MoveUp keeps the cursor's terminal display column (including across a run of
// repeated up/down operations), rather than its rune count.
func (e *Editor) MoveUp() bool {
	lineStart := e.lineStart(e.cursor)
	if lineStart == 0 {
		return false
	}
	if e.preferredCol < 0 {
		e.preferredCol = displayWidth(e.text[lineStart:e.cursor])
	}
	previousStart := e.lineStart(lineStart - 1)
	previousEnd := lineStart - 1
	e.cursor = cursorAtColumn(e.text, previousStart, previousEnd, e.preferredCol)
	return true
}

func (e *Editor) MoveDown() bool {
	lineStart := e.lineStart(e.cursor)
	lineEnd := e.lineEnd(e.cursor)
	if lineEnd == len(e.text) {
		return false
	}
	if e.preferredCol < 0 {
		e.preferredCol = displayWidth(e.text[lineStart:e.cursor])
	}
	nextStart := lineEnd + 1
	nextEnd := e.lineEnd(nextStart)
	e.cursor = cursorAtColumn(e.text, nextStart, nextEnd, e.preferredCol)
	return true
}

// KillToEnd deletes from the cursor to the line end. At an empty line end it
// deletes the following newline, matching the usual Ctrl-K behavior.
func (e *Editor) KillToEnd() string {
	end := e.lineEnd(e.cursor)
	if end == e.cursor && end < len(e.text) && e.text[end] == '\n' {
		end++
	}
	return e.kill(e.cursor, end)
}

func (e *Editor) KillToStart() string {
	return e.kill(e.lineStart(e.cursor), e.cursor)
}

func (e *Editor) KillWordBackward() string {
	start := e.cursor
	for start > 0 && !isWord(e.text[start-1]) && e.text[start-1] != '\n' {
		start--
	}
	for start > 0 && isWord(e.text[start-1]) {
		start--
	}
	return e.kill(start, e.cursor)
}

// Common names used by key dispatchers.
func (e *Editor) KillLine() string           { return e.KillToEnd() }
func (e *Editor) KillToBeginning() string    { return e.KillToStart() }
func (e *Editor) KillWord() string           { return e.KillWordBackward() }
func (e *Editor) DeleteWordBackward() string { return e.KillWordBackward() }

func (e *Editor) kill(start, end int) string {
	if start >= end {
		return ""
	}
	removed := string(e.text[start:end])
	copy(e.text[start:], e.text[end:])
	e.text = e.text[:len(e.text)-(end-start)]
	e.cursor = start
	e.leaveVerticalMovement()
	return removed
}

// SetHistory copies entries so the caller may safely reuse its slice.
func (e *Editor) SetHistory(entries []string) {
	e.history = append(e.history[:0], entries...)
	e.historyIndex = -1
	e.draft = ""
}

func (e *Editor) AddHistory(text string) {
	if text == "" {
		return
	}
	e.history = append(e.history, text)
	e.historyIndex = -1
	e.draft = ""
}

// HistoryPrevious selects older entries. The text present when navigation
// starts is retained as a draft and restored by HistoryNext.
func (e *Editor) HistoryPrevious() bool {
	if len(e.history) == 0 {
		return false
	}
	if e.historyIndex < 0 {
		e.draft = e.Text()
		e.historyIndex = len(e.history) - 1
	} else if e.historyIndex > 0 {
		e.historyIndex--
	} else {
		return false
	}
	e.loadHistory(e.history[e.historyIndex])
	return true
}

func (e *Editor) HistoryNext() bool {
	if e.historyIndex < 0 {
		return false
	}
	if e.historyIndex+1 < len(e.history) {
		e.historyIndex++
		e.loadHistory(e.history[e.historyIndex])
	} else {
		e.historyIndex = -1
		e.loadHistory(e.draft)
	}
	return true
}

func (e *Editor) PreviousHistory() bool { return e.HistoryPrevious() }
func (e *Editor) PrevHistory() bool     { return e.HistoryPrevious() }
func (e *Editor) HistoryPrev() bool     { return e.HistoryPrevious() }
func (e *Editor) NextHistory() bool     { return e.HistoryNext() }

func (e *Editor) loadHistory(s string) {
	e.text = []rune(s)
	e.cursor = len(e.text)
	e.leaveVerticalMovement()
}

// Clear removes editable text. It also exits history navigation but does not
// discard saved history.
func (e *Editor) Clear() {
	e.text = nil
	e.cursor = 0
	e.historyIndex = -1
	e.draft = ""
	e.leaveVerticalMovement()
}

func (e *Editor) lineStart(pos int) int {
	pos = clampEditor(pos, 0, len(e.text))
	for pos > 0 && e.text[pos-1] != '\n' {
		pos--
	}
	return pos
}

func (e *Editor) lineEnd(pos int) int {
	pos = clampEditor(pos, 0, len(e.text))
	for pos < len(e.text) && e.text[pos] != '\n' {
		pos++
	}
	return pos
}

func (e *Editor) leaveVerticalMovement() { e.preferredCol = -1 }

func isWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

func displayWidth(runes []rune) int {
	width := 0
	for _, r := range runes {
		width += editorRuneWidth(r)
	}
	return width
}

func cursorAtColumn(text []rune, start, end, column int) int {
	pos, width := start, 0
	for pos < end {
		w := editorRuneWidth(text[pos])
		if width+w > column {
			break
		}
		width += w
		pos++
	}
	return pos
}

func editorRuneWidth(r rune) int {
	if r == '\t' {
		return 1
	}
	if width := runewidth.RuneWidth(r); width > 0 {
		return width
	}
	return 0
}

func clampEditor(n, low, high int) int {
	if n < low {
		return low
	}
	if n > high {
		return high
	}
	return n
}
