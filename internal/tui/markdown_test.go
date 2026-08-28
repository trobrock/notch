package tui

import (
	"strings"
	"testing"
)

func markdownPlain(lines []string) string {
	plain := make([]string, len(lines))
	for i, line := range lines {
		plain[i] = plainANSI(line)
	}
	return strings.Join(plain, "\n")
}

func TestMarkdownSemanticBlocksAndInlineStyles(t *testing.T) {
	theme := DefaultTheme()
	source := "# Heading\n\nA **bold** and *italic* `code` with [site](https://example.com).\n\n" +
		"> quoted **text**\n\n- first\n  - nested\n1. ordered\n\n---\n\n```go\n  keep  spaces\n\tindent\n```"
	lines := renderMarkdown(source, 60, theme, theme.Text)
	plain := markdownPlain(lines)
	for _, want := range []string{"Heading", "A bold and italic code with site (https://example.com).", "│ quoted text", "• first", "• nested", "1. ordered", "───", "  keep  spaces", "    indent"} {
		if !strings.Contains(plain, want) {
			t.Errorf("rendered Markdown missing %q:\n%s", want, plain)
		}
	}
	for name, style := range map[string]string{
		"heading": theme.MarkdownHeading, "link": theme.MarkdownLink, "url": theme.MarkdownURL,
		"inline code": theme.MarkdownCode, "block code": theme.MarkdownCodeBlock,
		"quote": theme.MarkdownQuote, "rule": theme.MarkdownRule, "bullet": theme.MarkdownBullet,
	} {
		if style == "" || !strings.Contains(strings.Join(lines, "\n"), style) {
			t.Errorf("%s style %q not used", name, style)
		}
	}
	if !strings.Contains(strings.Join(lines, "\n"), "\x1b[1m") || !strings.Contains(strings.Join(lines, "\n"), "\x1b[3m") {
		t.Fatal("bold or italic SGR missing")
	}
}

func TestMarkdownWidthsUnicodeBreaksAndInjection(t *testing.T) {
	theme := DefaultTheme()
	input := "界界 **🙂🙂🙂** café  \nnext\n\n```\n  x  y\n```\n\x1b[31mred\x1b]0;title\a"
	for _, width := range []int{1, 2, 7, 16} {
		lines := renderMarkdown(input, width, theme, theme.Text)
		for i, line := range lines {
			if got := visibleWidth(line); got > width {
				t.Fatalf("width %d line %d has %d cells: %q", width, i, got, line)
			}
		}
		joined := strings.Join(lines, "\n")
		if strings.Contains(joined, "\x1b[31m") || strings.Contains(joined, "\x1b]0;title") {
			t.Fatalf("source terminal control survived at width %d: %q", width, joined)
		}
	}
	plain := markdownPlain(renderMarkdown(input, 40, theme, theme.Text))
	if !strings.Contains(plain, "界界 🙂🙂🙂 café") || !strings.Contains(plain, "next") || !strings.Contains(plain, "  x  y") {
		t.Fatalf("Unicode, hard break, or code whitespace lost:\n%s", plain)
	}
}

func TestMarkdownTables(t *testing.T) {
	theme := DefaultTheme()
	source := "| Name | Count | Note |\n| :--- | ---: | :---: |\n| apples | 12 | **fresh** |\n| pears | 3 | ripe fruit |"

	lines := renderMarkdown(source, 42, theme, theme.Text)
	plain := markdownPlain(lines)
	for _, want := range []string{"┌", "Name", "Count", "├", "apples", "12", "fresh", "└"} {
		if !strings.Contains(plain, want) {
			t.Errorf("rendered table missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, ":---") {
		t.Fatalf("table delimiter row was rendered literally:\n%s", plain)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "\x1b[1m") {
		t.Fatal("table header is not bold")
	}
	for i, line := range lines {
		if got := visibleWidth(line); got > 42 {
			t.Fatalf("table line %d has %d cells: %q", i, got, line)
		}
	}

	for _, width := range []int{1, 8, 12, 20} {
		for i, line := range renderMarkdown(source, width, theme, theme.Text) {
			if got := visibleWidth(line); got > width {
				t.Fatalf("width %d table line %d has %d cells: %q", width, i, got, line)
			}
		}
	}
}

func TestMarkdownStreamingIncompleteNeverPanics(t *testing.T) {
	inputs := []string{"*", "**unfinished", "[label](", "```go\nfunc f() {", "> **", "1. item\n   - ["}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("render panicked: %v", recovered)
				}
			}()
			lines := renderMarkdown(input, 9, DefaultTheme(), "")
			if len(lines) == 0 {
				t.Fatal("no output")
			}
			for _, line := range lines {
				if visibleWidth(line) > 9 {
					t.Fatalf("over-width line: %q", line)
				}
			}
		})
	}
}

func TestMarkdownWrapMovesWholeWords(t *testing.T) {
	theme := DefaultTheme()
	lines := renderMarkdown("alpha beta", 7, theme, theme.Text)
	if got := markdownPlain(lines); got != "alpha\nbeta" {
		t.Fatalf("wrapped words = %q", got)
	}
}

func TestMarkdownCacheIncludesTextWidthAndTheme(t *testing.T) {
	entry := TranscriptEntry{Kind: TranscriptAssistant, Text: "**cached** text"}
	theme := DefaultTheme()
	first := entry.markdown(20, entry.Text, theme, theme.Text)
	firstLine := &entry.cacheLines[0]
	second := entry.markdown(20, entry.Text, theme, theme.Text)
	if &second[0] != firstLine || &first[0] != &second[0] {
		t.Fatal("identical markdown render did not reuse cached lines")
	}
	entry.markdown(10, entry.Text, theme, theme.Text)
	if entry.cacheWidth != 10 || &entry.cacheLines[0] == firstLine {
		t.Fatal("width change did not invalidate Markdown cache")
	}
	oldStyle := entry.cacheStyle
	theme.MarkdownHeading = "\x1b[31m"
	entry.markdown(10, entry.Text, theme, theme.Text)
	if entry.cacheStyle == oldStyle {
		t.Fatal("theme change did not invalidate Markdown cache")
	}
	entry.Text += "!"
	entry.markdown(10, entry.Text, theme, theme.Text)
	if entry.cacheText != entry.Text {
		t.Fatal("text change did not invalidate Markdown cache")
	}
}

func TestUserMarkdownRestoresBackgroundAfterInlineReset(t *testing.T) {
	theme := DefaultTheme()
	state := &LayoutState{Width: 24, Height: 12, Transcript: []TranscriptEntry{{Kind: TranscriptUser, Text: "one **bold** `code`"}}}
	frame := BuildFrame(state)
	line := frame.Rows[1]
	if !strings.HasPrefix(line, theme.UserBG) {
		t.Fatalf("user line lacks background prefix: %q", line)
	}
	for _, resetAt := range strings.Split(line, theme.Reset)[1:] {
		if resetAt != "" && !strings.HasPrefix(resetAt, theme.UserBG) {
			// The final reset is followed only by end-of-line.
			t.Fatalf("inline reset did not restore user background: %q", line)
		}
	}
	if visibleWidth(line) != 24 {
		t.Fatalf("user line width = %d", visibleWidth(line))
	}
}

func BenchmarkRenderMarkdown(b *testing.B) {
	theme := DefaultTheme()
	source := "# Heading\n\nA **representative** paragraph with [a link](https://example.com), `code`, and 世界.\n\n- one\n- two\n\n```go\nfmt.Println(\"hello\")\n```"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		renderMarkdown(source, 80, theme, theme.Text)
	}
}

func BenchmarkRenderMarkdownCached(b *testing.B) {
	entry := TranscriptEntry{Kind: TranscriptAssistant, Text: "# Heading\n\nA **representative** paragraph with [a link](https://example.com), `code`, and 世界.\n\n- one\n- two\n\n```go\nfmt.Println(\"hello\")\n```"}
	theme := DefaultTheme()
	entry.markdown(80, entry.Text, theme, theme.Text)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry.markdown(80, entry.Text, theme, theme.Text)
	}
}
