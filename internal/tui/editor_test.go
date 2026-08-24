package tui

import "testing"

func TestEditorRuneAwareEditing(t *testing.T) {
	e := NewEditor()
	e.Insert("a界🙂")
	assertEditor(t, e, "a界🙂", 3)
	e.MoveLeft()
	if !e.Backspace() {
		t.Fatal("Backspace reported false")
	}
	assertEditor(t, e, "a🙂", 1)
	if !e.Delete() {
		t.Fatal("Delete reported false")
	}
	assertEditor(t, e, "a", 1)
	e.SetCursor(-10)
	assertEditor(t, e, "a", 0)
	e.Insert("é")
	assertEditor(t, e, "éa", 1)
	e.SetCursor(99)
	assertEditor(t, e, "éa", 2)
}

func TestEditorMovement(t *testing.T) {
	tests := []struct {
		name string
		text string
		at   int
		move func(*Editor)
		want int
	}{
		{"left", "abc", 2, func(e *Editor) { e.MoveLeft() }, 1},
		{"right", "abc", 2, func(e *Editor) { e.MoveRight() }, 3},
		{"home first line", "abc\ndef", 2, func(e *Editor) { e.MoveHome() }, 0},
		{"home second line", "abc\ndef", 6, func(e *Editor) { e.MoveHome() }, 4},
		{"end first line", "abc\ndef", 1, func(e *Editor) { e.MoveEnd() }, 3},
		{"word left", "one,  two", 9, func(e *Editor) { e.MoveWordLeft() }, 6},
		{"word left separator", "one,  two", 6, func(e *Editor) { e.MoveWordLeft() }, 0},
		{"word right", "one,  two", 0, func(e *Editor) { e.MoveWordRight() }, 3},
		{"word right separator", "one,  two", 3, func(e *Editor) { e.MoveWordRight() }, 9},
		{"unicode word", "猫  café", 7, func(e *Editor) { e.MoveWordLeft() }, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewEditor()
			e.SetText(tt.text)
			e.SetCursor(tt.at)
			tt.move(e)
			if got := e.Cursor(); got != tt.want {
				t.Fatalf("cursor = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEditorMultilineMovementUsesDisplayColumns(t *testing.T) {
	// Rune columns differ from terminal columns: 界 occupies two display cells.
	e := NewEditor()
	e.SetText("ab界z\n123456\nx")
	e.SetCursor(3) // after ab界, display column 4
	if !e.MoveDown() {
		t.Fatal("MoveDown failed")
	}
	assertEditor(t, e, "ab界z\n123456\nx", 9) // second line, column 4
	if !e.MoveDown() {
		t.Fatal("second MoveDown failed")
	}
	assertEditor(t, e, "ab界z\n123456\nx", 13) // short final line
	if !e.MoveUp() {
		t.Fatal("MoveUp failed")
	}
	// The preferred column remains 4 despite having visited the short line.
	assertEditor(t, e, "ab界z\n123456\nx", 9)

	// A target column cannot land in the middle of a wide rune.
	e.SetText("ab\na界z")
	e.SetCursor(2)
	e.MoveDown()
	assertEditor(t, e, "ab\na界z", 4) // before 界 (columns jump 1 -> 3)
}

func TestEditorKillOperations(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		at         int
		kill       func(*Editor) string
		wantKilled string
		wantText   string
		wantCursor int
	}{
		{"to end", "abc def\nnext", 3, func(e *Editor) string { return e.KillToEnd() }, " def", "abc\nnext", 3},
		{"newline at end", "abc\nnext", 3, func(e *Editor) string { return e.KillToEnd() }, "\n", "abcnext", 3},
		{"to start", "abc def\nnext", 5, func(e *Editor) string { return e.KillToStart() }, "abc d", "ef\nnext", 0},
		{"word backward", "one,  two", 9, func(e *Editor) string { return e.KillWordBackward() }, "two", "one,  ", 6},
		{"word plus spaces", "one,  two", 6, func(e *Editor) string { return e.KillWordBackward() }, "one,  ", "two", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewEditor()
			e.SetText(tt.text)
			e.SetCursor(tt.at)
			if got := tt.kill(e); got != tt.wantKilled {
				t.Fatalf("killed %q, want %q", got, tt.wantKilled)
			}
			assertEditor(t, e, tt.wantText, tt.wantCursor)
		})
	}
}

func TestEditorHistoryPreservesDraft(t *testing.T) {
	e := NewEditor([]string{"first", "second"})
	e.Insert("unfinished")
	if !e.HistoryPrevious() {
		t.Fatal("first previous failed")
	}
	assertEditor(t, e, "second", 6)
	if !e.HistoryPrevious() {
		t.Fatal("second previous failed")
	}
	assertEditor(t, e, "first", 5)
	if e.HistoryPrevious() {
		t.Fatal("previous moved before oldest entry")
	}
	if !e.HistoryNext() {
		t.Fatal("next failed")
	}
	assertEditor(t, e, "second", 6)
	if !e.HistoryNext() {
		t.Fatal("draft restore failed")
	}
	assertEditor(t, e, "unfinished", 10)
	if e.HistoryNext() {
		t.Fatal("next moved past draft")
	}
}

func TestEditorClearKeepsHistory(t *testing.T) {
	e := NewEditor([]string{"saved"})
	e.SetText("current")
	e.Clear()
	assertEditor(t, e, "", 0)
	if !e.HistoryPrevious() {
		t.Fatal("Clear discarded history")
	}
	assertEditor(t, e, "saved", 5)
}

func assertEditor(t *testing.T, e *Editor, text string, cursor int) {
	t.Helper()
	if got := e.Text(); got != text {
		t.Fatalf("Text() = %q, want %q", got, text)
	}
	if got := e.Cursor(); got != cursor {
		t.Fatalf("Cursor() = %d, want %d", got, cursor)
	}
}
