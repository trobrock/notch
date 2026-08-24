package tui

import (
	"reflect"
	"strings"
	"testing"
)

func TestThemeNamesAndAliases(t *testing.T) {
	want := []string{"catppuccin-mocha", "dark", "dracula"}
	if got := ThemeNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ThemeNames = %#v, want %#v", got, want)
	}
	aliases := map[string]string{
		"DARK": "dark", " pi-dark ": "dark", "DrAcUlA": "dracula",
		"catppuccin": "catppuccin-mocha", "Catppuccin Mocha": "catppuccin-mocha",
		"catppuccin_mocha": "catppuccin-mocha", "ctp-mocha": "catppuccin-mocha", "mocha": "catppuccin-mocha",
	}
	for alias, canonical := range aliases {
		got, ok := ThemeByName(alias)
		wantTheme, _ := ThemeByName(canonical)
		if !ok || !reflect.DeepEqual(got, wantTheme) {
			t.Errorf("ThemeByName(%q) did not resolve to %q", alias, canonical)
		}
	}
	if _, ok := ThemeByName("unknown"); ok {
		t.Fatal("unknown theme unexpectedly resolved")
	}
}

func TestDarkThemeMatchesPiDarkJSON(t *testing.T) {
	theme, ok := ThemeByName("dark")
	if !ok {
		t.Fatal("dark theme missing")
	}
	checks := map[string][2]string{
		"accent":           {theme.Accent, "\x1b[38;2;138;190;183m"},
		"border":           {theme.Border, "\x1b[38;2;95;135;255m"},
		"muted":            {theme.Muted, "\x1b[38;2;128;128;128m"},
		"markdown heading": {theme.MarkdownHeading, "\x1b[38;2;240;198;116m"},
		"markdown link":    {theme.MarkdownLink, "\x1b[38;2;129;162;190m"},
		"markdown URL":     {theme.MarkdownURL, "\x1b[38;2;102;102;102m"},
		"markdown code":    {theme.MarkdownCode, "\x1b[38;2;222;147;95m"},
		"markdown block":   {theme.MarkdownCodeBlock, "\x1b[38;2;181;189;104m"},
		"markdown quote":   {theme.MarkdownQuote, "\x1b[38;2;102;102;102m"},
		"markdown bullet":  {theme.MarkdownBullet, "\x1b[38;2;138;190;183m"},
		"user bg":          {theme.UserBG, "\x1b[48;2;52;53;65m"},
		"pending bg":       {theme.ToolPendingBG, "\x1b[48;2;40;40;50m"},
		"success bg":       {theme.ToolSuccessBG, "\x1b[48;2;40;50;40m"},
		"error bg":         {theme.ToolErrorBG, "\x1b[48;2;60;40;40m"},
		"tool output":      {theme.ToolOutput, "\x1b[38;2;128;128;128m"},
		"notice":           {theme.Notice, "\x1b[38;2;255;255;0m"},
		"error":            {theme.Error, "\x1b[38;2;204;102;102m"},
		"thinking minimal": {theme.ThinkingMinimal, "\x1b[38;2;110;110;110m"},
		"thinking high":    {theme.ThinkingHigh, "\x1b[38;2;178;148;187m"},
		"thinking xhigh":   {theme.ThinkingXHigh, "\x1b[38;2;209;131;232m"},
	}
	for name, check := range checks {
		if check[0] != check[1] {
			t.Errorf("%s = %q, want %q", name, check[0], check[1])
		}
	}
	if theme.Text != "" || theme.UserText != "" {
		t.Fatalf("Pi dark must retain terminal default text foreground: text=%q user=%q", theme.Text, theme.UserText)
	}
}

func TestStandardPaletteThemeColors(t *testing.T) {
	dracula, _ := ThemeByName("dracula")
	for name, check := range map[string][2]string{
		"Dracula text":    {dracula.Text, "\x1b[38;2;248;248;242m"},
		"Dracula accent":  {dracula.Accent, "\x1b[38;2;139;233;253m"},
		"Dracula user":    {dracula.UserBG, "\x1b[48;2;68;71;90m"},
		"Dracula error":   {dracula.Error, "\x1b[38;2;255;85;85m"},
		"Dracula heading": {dracula.MarkdownHeading, "\x1b[38;2;241;250;140m"},
		"Dracula code":    {dracula.MarkdownCodeBlock, "\x1b[38;2;80;250;123m"},
	} {
		if check[0] != check[1] {
			t.Errorf("%s = %q, want %q", name, check[0], check[1])
		}
	}

	mocha, _ := ThemeByName("catppuccin-mocha")
	for name, check := range map[string][2]string{
		"Mocha text":    {mocha.Text, "\x1b[38;2;205;214;244m"},
		"Mocha accent":  {mocha.Accent, "\x1b[38;2;137;180;250m"},
		"Mocha user":    {mocha.UserBG, "\x1b[48;2;49;50;68m"},
		"Mocha error":   {mocha.Error, "\x1b[38;2;243;139;168m"},
		"Mocha heading": {mocha.MarkdownHeading, "\x1b[38;2;249;226;175m"},
		"Mocha code":    {mocha.MarkdownCodeBlock, "\x1b[38;2;166;227;161m"},
	} {
		if check[0] != check[1] {
			t.Errorf("%s = %q, want %q", name, check[0], check[1])
		}
	}
}

func TestThemeNameAndThinkingLevelAffectFrameWithoutPageBackground(t *testing.T) {
	state := &LayoutState{Width: 30, Height: 8, ThemeName: "dracula", ThinkingLevel: "low", Editor: layoutEditor("x", 1)}
	low := BuildFrame(state)
	dracula, _ := ThemeByName("dracula")
	if !strings.HasPrefix(low.Rows[3], dracula.ThinkingLow) || !strings.HasPrefix(low.Rows[5], dracula.ThinkingLow) {
		t.Fatalf("low-thinking borders have wrong style: %q / %q", low.Rows[3], low.Rows[5])
	}
	// Unused transcript cells are intentionally terminal-default, not a themed page background.
	if strings.Contains(low.Rows[0], "\x1b[48;") {
		t.Fatalf("page background was forced: %q", low.Rows[0])
	}
	state.ThinkingLevel = "xhigh"
	xhigh := BuildFrame(state)
	if !strings.HasPrefix(xhigh.Rows[3], dracula.ThinkingXHigh) || low.Rows[3] == xhigh.Rows[3] {
		t.Fatalf("xhigh did not change editor border: %q", xhigh.Rows[3])
	}
}

func TestPartialLegacyThemeCompatibility(t *testing.T) {
	state := &LayoutState{Width: 20, Height: 10,
		Theme:      Theme{Assistant: "\x1b[31m", User: "\x1b[32m", Status: "\x1b[33m", Reset: "\x1b[0m"},
		Transcript: []TranscriptEntry{{Kind: TranscriptAssistant, Text: "answer"}}, Editor: layoutEditor("", 0), CWD: "/tmp",
	}
	frame := BuildFrame(state)
	if !strings.HasPrefix(frame.Rows[0], "\x1b[31m") {
		t.Fatalf("legacy Assistant override ignored: %q", frame.Rows[0])
	}
	if !strings.HasPrefix(frame.Rows[len(frame.Rows)-1], "\x1b[33m") {
		t.Fatalf("legacy Status override ignored: %q", frame.Rows[len(frame.Rows)-1])
	}
}
