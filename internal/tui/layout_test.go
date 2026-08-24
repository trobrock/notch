package tui

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func layoutEditor(text string, cursor int) *Editor {
	e := NewEditor()
	e.SetText(text)
	e.SetCursor(cursor)
	return e
}

func plainANSI(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		if n := ansiSequenceLen(s); n > 0 {
			s = s[n:]
			continue
		}
		r, n := utf8.DecodeRuneInString(s)
		b.WriteRune(r)
		s = s[n:]
	}
	return b.String()
}

func TestBuildFramePiRowRoles(t *testing.T) {
	theme := DefaultTheme()
	state := &LayoutState{
		Width: 80, Height: 28,
		Transcript: []TranscriptEntry{
			{Kind: TranscriptUser, Text: "hello"},
			{Kind: TranscriptAssistant, Text: "answer"},
			{Kind: TranscriptTool, Label: "read", Text: "waiting", Pending: true},
			{Kind: TranscriptTool, Label: "write", Text: "saved"},
			{Kind: TranscriptTool, Label: "shell", Text: "failed", Error: true},
		},
		Editor: layoutEditor("compose", 7), CWD: "/work/notch", GitBranch: "main",
		Status: "ready", Usage: "↑1.2k ↓42", ContextTokens: 25000, ContextWindow: 200000,
		AutoCompact: true, Provider: "anthropic", Model: "sonnet", ThinkingLevel: "high",
	}
	frame := BuildFrame(state)

	// User cards include colored padding above and below and no speaker label.
	for _, row := range []int{0, 1, 2} {
		if !strings.HasPrefix(frame.Rows[row], theme.UserBG) {
			t.Fatalf("user row %d missing full-width background: %q", row, frame.Rows[row])
		}
	}
	if got := plainANSI(frame.Rows[1]); !strings.HasPrefix(got, " hello") || strings.Contains(got, "you:") {
		t.Fatalf("user content = %q", got)
	}
	if strings.TrimSpace(plainANSI(frame.Rows[0])) != "" || strings.TrimSpace(plainANSI(frame.Rows[2])) != "" {
		t.Fatal("user card padding rows are not blank")
	}

	if strings.TrimSpace(plainANSI(frame.Rows[3])) != "" {
		t.Fatalf("user/assistant separator is not blank: %q", frame.Rows[3])
	}
	if got := plainANSI(frame.Rows[4]); !strings.HasPrefix(got, " answer") || strings.Contains(frame.Rows[4], "\x1b[48;") {
		t.Fatalf("assistant must be plain and left padded: %q", frame.Rows[4])
	}
	if strings.TrimSpace(plainANSI(frame.Rows[5])) != "" {
		t.Fatalf("assistant spacing row is not blank: %q", frame.Rows[5])
	}

	checks := []struct {
		row  int
		bg   string
		text string
	}{
		{6, theme.ToolPendingBG, ""}, {7, theme.ToolPendingBG, " ● read"}, {8, theme.ToolPendingBG, " │ waiting"}, {9, theme.ToolPendingBG, ""},
		{11, theme.ToolSuccessBG, ""}, {12, theme.ToolSuccessBG, " ✓ write"}, {13, theme.ToolSuccessBG, " │ saved"}, {14, theme.ToolSuccessBG, ""},
		{16, theme.ToolErrorBG, ""}, {17, theme.ToolErrorBG, " ✗ shell"}, {18, theme.ToolErrorBG, " │ failed"}, {19, theme.ToolErrorBG, ""},
	}
	for _, check := range checks {
		if !strings.HasPrefix(frame.Rows[check.row], check.bg) || !strings.HasPrefix(plainANSI(frame.Rows[check.row]), check.text) {
			t.Errorf("tool row %d role mismatch: %q", check.row, frame.Rows[check.row])
		}
	}

	// One-line composer occupies the last five rows: border, content, border,
	// cwd, then status/model. Both borders use the selected thinking color.
	if !strings.HasPrefix(frame.Rows[23], theme.ThinkingHigh) || !strings.Contains(frame.Rows[23], "─") ||
		!strings.HasPrefix(frame.Rows[25], theme.ThinkingHigh) || !strings.Contains(frame.Rows[25], "─") {
		t.Fatalf("composer borders do not use thinking-high: %q / %q", frame.Rows[23], frame.Rows[25])
	}
	if got := plainANSI(frame.Rows[24]); !strings.HasPrefix(got, " compose") || strings.Contains(got, "›") {
		t.Fatalf("composer content = %q", got)
	}
	if got := strings.TrimRight(plainANSI(frame.Rows[26]), " "); got != "/work/notch (main)" {
		t.Fatalf("cwd footer = %q", got)
	}
	last := plainANSI(frame.Rows[27])
	if !strings.Contains(last, "↑1.2k ↓42 12.5%/200k (auto)") || strings.Contains(last, "ready") || !strings.HasSuffix(last, "(anthropic) sonnet • high") {
		t.Fatalf("stats footer = %q", last)
	}
	if frame.CursorRow != 24 || frame.CursorCol != 8 {
		t.Fatalf("cursor = (%d,%d), want (24,8)", frame.CursorRow, frame.CursorCol)
	}
}

func TestBuildFrameWidthsUnicodeAndTinyTerminals(t *testing.T) {
	for _, width := range []int{1, 2, 8, 20, 80} {
		for height := 1; height <= 7; height++ {
			name := strconv.Itoa(width) + "x" + strconv.Itoa(height)
			t.Run(name, func(t *testing.T) {
				state := &LayoutState{Width: width, Height: height,
					Transcript: []TranscriptEntry{{Kind: TranscriptAssistant, Text: "界界 café e\u0301 🙂🙂"}},
					Editor:     layoutEditor("你好🙂abc", 3), Status: "busy", Provider: "p", Model: "m",
				}
				frame := BuildFrame(state)
				if len(frame.Rows) != height {
					t.Fatalf("rows = %d", len(frame.Rows))
				}
				for row, line := range frame.Rows {
					if got := visibleWidth(line); got != width {
						t.Fatalf("row %d width = %d, want %d: %q", row, got, width, line)
					}
				}
				if frame.CursorRow < 0 || frame.CursorRow >= height || frame.CursorCol < 0 || frame.CursorCol >= width {
					t.Fatalf("cursor out of bounds: %+v", frame)
				}
			})
		}
	}
}

func TestUnicodeComposerCursorUsesOneCellInset(t *testing.T) {
	state := &LayoutState{Width: 20, Height: 9, Editor: layoutEditor("你好🙂abc", 3)}
	frame := BuildFrame(state)
	if !frame.CursorVisible || frame.CursorCol != 1+visibleWidth("你好🙂") {
		t.Fatalf("unicode cursor = (%d,%d), visible=%v", frame.CursorRow, frame.CursorCol, frame.CursorVisible)
	}
}

func TestComposerMaximumEightVisibleLinesAndFollowsCursor(t *testing.T) {
	text := strings.Repeat("abcdefghij", 24)
	state := &LayoutState{Width: 20, Height: 20, Editor: layoutEditor(text, len([]rune(text)))}
	frame := BuildFrame(state)
	borders := make([]int, 0, 2)
	for i, line := range frame.Rows {
		if strings.Contains(line, "─") {
			borders = append(borders, i)
		}
	}
	if len(borders) != 2 || borders[1]-borders[0]-1 != 8 {
		t.Fatalf("border rows = %v, want eight editor rows between", borders)
	}
	if !frame.CursorVisible || frame.CursorRow != borders[1]-1 {
		t.Fatalf("cursor not kept in visible editor tail: %+v", frame)
	}
}

func TestTranscriptStartsAtTopAndWrapCacheIsReused(t *testing.T) {
	state := &LayoutState{Width: 20, Height: 12, Transcript: []TranscriptEntry{{Kind: TranscriptAssistant, Text: "one two three four five six"}}}
	frame := BuildFrame(state)
	if !strings.HasPrefix(plainANSI(frame.Rows[0]), " one") {
		t.Fatalf("short transcript was not top aligned: %q", frame.Rows[0])
	}
	entry := &state.Transcript[0]
	first := &entry.cacheLines[0]
	BuildFrame(state)
	if first != &entry.cacheLines[0] {
		t.Fatal("unchanged frame did not reuse cached wrapping")
	}
	state.Width = 80
	BuildFrame(state)
	if entry.cacheWidth != 79 {
		t.Fatalf("assistant cache width = %d, want 79", entry.cacheWidth)
	}
}

func TestTranscriptSpacingAndToolDetailVisuals(t *testing.T) {
	theme := DefaultTheme()
	state := &LayoutState{Width: 32, Height: 30, Transcript: []TranscriptEntry{
		{Kind: TranscriptUser, Text: "**user**"},
		{Kind: TranscriptAssistant, Text: "assistant"},
		{Kind: TranscriptTool, Label: "shell", Detail: `cmd="go test"`, Text: "ok"},
	}}
	lines := renderTranscript(state, state.Width, completeTheme(theme, ""))
	// User internal padding is rows 0 and 2; the unstyled row 3 is the
	// inter-entry separator. Assistant/tool have exactly one separator too.
	if len(lines) != 10 {
		t.Fatalf("transcript rows = %d, want 10: %#v", len(lines), lines)
	}
	for _, row := range []int{0, 1, 2} {
		if !strings.HasPrefix(lines[row], theme.UserBG) {
			t.Errorf("user card row %d is not filled: %q", row, lines[row])
		}
	}
	for _, row := range []int{3, 5} {
		if strings.TrimSpace(plainANSI(lines[row])) != "" || strings.Contains(lines[row], "\x1b[48;") {
			t.Errorf("separator row %d is not one normal blank: %q", row, lines[row])
		}
	}
	for _, row := range []int{6, 7, 8, 9} {
		if !strings.HasPrefix(lines[row], theme.ToolSuccessBG) || visibleWidth(lines[row]) != state.Width {
			t.Errorf("tool row %d lacks full success background/width: %q", row, lines[row])
		}
	}
	if got := plainANSI(lines[7]); !strings.Contains(got, "✓ shell") || !strings.Contains(got, `cmd="go test"`) {
		t.Fatalf("tool title/detail = %q", got)
	}
	if got := plainANSI(lines[8]); !strings.Contains(got, "│ ok") {
		t.Fatalf("tool output = %q", got)
	}
}

func TestPendingMessagesRenderAboveComposer(t *testing.T) {
	state := &LayoutState{Width: 60, Height: 12, Editor: layoutEditor("draft", 5), PendingMessages: []PendingMessage{
		{ID: "1", Mode: "steer", Text: "change direction"},
		{ID: "2", Mode: "follow_up", Text: "then summarize"},
	}}
	frame := BuildFrame(state)
	plain := make([]string, len(frame.Rows))
	for i, row := range frame.Rows {
		plain[i] = plainANSI(row)
	}
	joined := strings.Join(plain, "\n")
	if !strings.Contains(joined, "↪ Steering queued: change direction") || !strings.Contains(joined, "↳ Follow-up queued: then summarize") {
		t.Fatalf("pending queue:\n%s", joined)
	}
}

func TestCommandSuggestionsRenderAboveComposer(t *testing.T) {
	state := &LayoutState{
		Width: 60, Height: 14, Editor: layoutEditor("/th", 3),
		CommandSuggestions: []CommandSuggestion{
			{Name: "theme", ArgumentHint: "[name]", Description: "select theme"},
			{Name: "thinking", ArgumentHint: "[level]", Description: "set thinking"},
		},
		CommandSelection: 1,
	}
	frame := BuildFrame(state)
	plain := make([]string, len(frame.Rows))
	for i, row := range frame.Rows {
		plain[i] = plainANSI(row)
	}
	joined := strings.Join(plain, "\n")
	if !strings.Contains(joined, "  /theme [name] — select theme") || !strings.Contains(joined, "› /thinking [level] — set thinking") {
		t.Fatalf("suggestion menu:\n%s", joined)
	}
	if !frame.CursorVisible {
		t.Fatal("composer cursor hidden by suggestions")
	}
}

func TestThinkingIndicatorAndSummaryRendering(t *testing.T) {
	theme := DefaultTheme()
	state := &LayoutState{Transcript: []TranscriptEntry{{Kind: KindThinking, Pending: true}}}
	pending := renderTranscript(state, 40, theme)
	if got := plainANSI(strings.Join(pending, "\n")); !strings.Contains(got, "◐ Thinking…") {
		t.Fatalf("pending thinking = %q", got)
	}
	state.ThinkingFrame = 1
	pending = renderTranscript(state, 40, theme)
	if got := plainANSI(strings.Join(pending, "\n")); !strings.Contains(got, "◓ Thinking…") {
		t.Fatalf("animated thinking = %q", got)
	}
	summary := renderTranscript(&LayoutState{Transcript: []TranscriptEntry{{Kind: KindThinking, Text: "Checked **three** files."}}}, 40, theme)
	plain := plainANSI(strings.Join(summary, "\n"))
	if !strings.Contains(plain, "◆ Thinking") || !strings.Contains(plain, "Checked three files.") || !strings.Contains(strings.Join(summary, "\n"), "\x1b[1m") {
		t.Fatalf("thinking summary = %q", plain)
	}
}

func TestScrollOffsetShowsIndicator(t *testing.T) {
	entries := make([]TranscriptEntry, 20)
	for i := range entries {
		entries[i] = TranscriptEntry{Kind: TranscriptAssistant, Text: "line"}
	}
	state := &LayoutState{Width: 20, Height: 10, Transcript: entries, ScrollOffset: 3}
	frame := BuildFrame(state)
	if !strings.Contains(frame.Rows[0], "↑ ↓ 3") {
		t.Fatalf("no bidirectional scroll indicator in %q", frame.Rows[0])
	}
	state.ScrollOffset = transcriptScrollLimit(state)
	frame = BuildFrame(state)
	if strings.Contains(frame.Rows[0], "↑") || !strings.Contains(frame.Rows[0], "↓") {
		t.Fatalf("wrong oldest-position indicator in %q", frame.Rows[0])
	}
}

func TestCompactTokenFormatting(t *testing.T) {
	cases := map[int]string{999: "999", 1000: "1.0k", 9999: "10.0k", 10000: "10k", 999999: "1000k", 1000000: "1.0M", 10000000: "10M"}
	for in, want := range cases {
		if got := formatTokens(in); got != want {
			t.Errorf("formatTokens(%d) = %q, want %q", in, got, want)
		}
	}
}

func BenchmarkBuildFrame(b *testing.B) {
	entries := make([]TranscriptEntry, 200)
	for i := range entries {
		kind := TranscriptAssistant
		if i%8 == 0 {
			kind = TranscriptUser
		}
		if i%11 == 0 {
			kind = TranscriptTool
		}
		entries[i] = TranscriptEntry{Kind: kind, Label: "read", Text: "This is a representative transcript line with Unicode 世界 and enough words to wrap."}
	}
	state := &LayoutState{Width: 80, Height: 40, Transcript: entries, Editor: layoutEditor("benchmark", 9), Provider: "provider", Model: "model", CWD: "/work/notch", ContextTokens: 32000, ContextWindow: 200000, AutoCompact: true, ThinkingLevel: "high", ThemeName: "catppuccin-mocha"}
	BuildFrame(state)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		BuildFrame(state)
	}
}

func TestUserTranscriptPreservesTypedNewlines(t *testing.T) {
	state := &LayoutState{Width: 40, Height: 12, Editor: NewEditor(), Transcript: []TranscriptEntry{{Kind: KindUser, Text: "first line\nsecond line\n\nfourth line"}}}
	lines := renderTranscript(state, state.Width, completeTheme(Theme{}, ""))
	if len(lines) < 6 {
		t.Fatalf("rendered lines = %#v", lines)
	}
	var first, second, fourth int = -1, -1, -1
	for i, line := range lines {
		plain := sanitiseTerminalText(line)
		if strings.Contains(plain, "first line") {
			first = i
		}
		if strings.Contains(plain, "second line") {
			second = i
		}
		if strings.Contains(plain, "fourth line") {
			fourth = i
		}
	}
	if second != first+1 || fourth != second+2 {
		t.Fatalf("rendered lines did not preserve newlines: %#v", lines)
	}
}
