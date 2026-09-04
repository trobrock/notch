package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/session"
)

func TestRecentSessionMatchesPreviewModelAndID(t *testing.T) {
	infos := []session.Info{
		{Preview: "Database migration", Header: session.Header{Model: "sonnet"}},
		{Preview: "Fix oauth callback", Header: session.Header{ID: "session-123"}},
	}
	for query, want := range map[string]int{"migration": 0, "sonnet": 0, "oauth": 1, "session-123": 1} {
		got := recentSessionMatches(infos, query)
		if len(got) != 1 || got[0] != want {
			t.Fatalf("matches(%q) = %v, want [%d]", query, got, want)
		}
	}
}

func TestRecentSessionPickerRowsAreCompactAndWidthSafe(t *testing.T) {
	theme := completeTheme(DefaultTheme(), "")
	infos := []session.Info{{
		Preview:    strings.Repeat("界", 80) + "\x1b[31munsafe",
		ModifiedAt: time.Now().Add(-2 * time.Hour), MessageCount: 12,
	}}
	rows, cursor := recentSessionPickerRows(infos, []int{0}, "auth", 0, 40, theme)
	if len(rows) != 3 {
		t.Fatalf("row count = %d, want 3", len(rows))
	}
	for i, row := range rows {
		if got := visibleWidth(row); got != 40 {
			t.Fatalf("row %d width = %d, want 40: %q", i, got, row)
		}
	}
	text := strings.Join(rows, "\n")
	for _, want := range []string{"auth", "2h", "12 msgs", "enter resume", "1/1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("picker rows missing %q: %q", want, text)
		}
	}
	if strings.Contains(text, "[31m") {
		t.Fatalf("picker retained terminal escape payload: %q", text)
	}
	if cursor != 6 {
		t.Fatalf("cursor column = %d, want 6", cursor)
	}
}
