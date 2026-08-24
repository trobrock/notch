package tui

import (
	"fmt"
	"sort"
	"strings"
)

// Theme contains semantic ANSI styles used by the fullscreen renderer. A style
// is an SGR prefix, not a color name. BuildFrame fills empty fields from the
// selected built-in theme, preserving the partial-theme behavior of older
// callers.
type Theme struct {
	Text   string
	Muted  string
	Accent string
	Border string

	// Markdown styles are foreground/style prefixes.  The Markdown-prefixed
	// names are canonical; the shorter aliases keep custom themes pleasant to
	// construct and are filled in both directions by completeTheme.
	MarkdownHeading   string
	MarkdownLink      string
	MarkdownURL       string
	MarkdownCode      string
	MarkdownCodeBlock string
	MarkdownQuote     string
	MarkdownRule      string
	MarkdownBullet    string
	Heading           string
	Link              string
	LinkURL           string
	LinkUrl           string // Pi's TypeScript spelling, retained as a Go alias.
	InlineCode        string
	Code              string
	CodeBlock         string
	CodeBlockBorder   string
	BlockQuote        string
	Quote             string
	QuoteBorder       string
	HorizontalRule    string
	HR                string
	ListBullet        string

	UserBG   string
	UserText string

	ToolPendingBG string
	ToolSuccessBG string
	ToolErrorBG   string
	ToolTitle     string
	ToolOutput    string

	Notice   string
	Error    string
	Composer string
	Footer   string

	ThinkingOff     string
	ThinkingMinimal string
	ThinkingLow     string
	ThinkingMedium  string
	ThinkingHigh    string
	ThinkingXHigh   string

	// Deprecated compatibility styles. They remain usable by callers that
	// constructed Theme values before semantic themes were introduced.
	User      string
	Assistant string
	Tool      string
	Pending   string
	Status    string
	Reset     string
}

// DefaultTheme returns Pi's built-in dark theme.
func DefaultTheme() Theme {
	t, _ := ThemeByName("dark")
	return t
}

// ThemeNames returns the canonical built-in names in stable alphabetical order.
func ThemeNames() []string {
	names := make([]string, 0, len(builtinThemes))
	for name := range builtinThemes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ThemeByName looks up a built-in theme. Names are case-insensitive; spaces and
// underscores are treated as hyphens, and common Catppuccin Mocha aliases are
// accepted.
func ThemeByName(name string) (Theme, bool) {
	name = normalizeThemeName(name)
	if name == "" || name == "default" || name == "pi" || name == "pi-dark" {
		name = "dark"
	}
	switch name {
	case "catppuccin", "mocha", "catppuccinmocha", "ctp-mocha", "ctpmocha":
		name = "catppuccin-mocha"
	}
	t, ok := builtinThemes[name]
	return t, ok
}

func normalizeThemeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.NewReplacer("_", "-", " ", "-").Replace(name)
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	return name
}

func fg(hex string) string {
	if hex == "" {
		return ""
	}
	var r, g, b uint8
	if _, err := fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b); err != nil {
		panic("invalid built-in theme color " + hex)
	}
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
}

func bg(hex string) string {
	if hex == "" {
		return ""
	}
	var r, g, b uint8
	if _, err := fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b); err != nil {
		panic("invalid built-in theme color " + hex)
	}
	return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", r, g, b)
}

func semanticTheme(c themeColors) Theme {
	t := Theme{
		Text: c.text, Muted: c.muted, Accent: c.accent, Border: c.border,
		MarkdownHeading: c.mdHeading, MarkdownLink: c.mdLink, MarkdownURL: c.mdURL,
		MarkdownCode: c.mdCode, MarkdownCodeBlock: c.mdCodeBlock, MarkdownQuote: c.mdQuote,
		MarkdownRule: c.mdRule, MarkdownBullet: c.mdBullet,
		UserBG: c.userBG, UserText: c.userText,
		ToolPendingBG: c.toolPendingBG, ToolSuccessBG: c.toolSuccessBG, ToolErrorBG: c.toolErrorBG,
		ToolTitle: c.toolTitle, ToolOutput: c.toolOutput,
		Notice: c.notice, Error: c.err, Composer: c.composer, Footer: c.footer,
		ThinkingOff: c.thinkingOff, ThinkingMinimal: c.thinkingMinimal,
		ThinkingLow: c.thinkingLow, ThinkingMedium: c.thinkingMedium,
		ThinkingHigh: c.thinkingHigh, ThinkingXHigh: c.thinkingXHigh,
		Reset: "\x1b[0m",
	}
	// Populate old names too, so reading a built-in through the original API is
	// useful and old partial themes still have intuitive fallback values.
	t.User, t.Assistant, t.Tool = t.UserText, t.Text, t.ToolTitle
	t.Pending, t.Status = t.Muted, t.Footer
	t.Heading, t.Link, t.LinkURL, t.LinkUrl = t.MarkdownHeading, t.MarkdownLink, t.MarkdownURL, t.MarkdownURL
	t.InlineCode, t.Code, t.CodeBlock, t.CodeBlockBorder = t.MarkdownCode, t.MarkdownCode, t.MarkdownCodeBlock, t.MarkdownQuote
	t.BlockQuote, t.Quote, t.QuoteBorder = t.MarkdownQuote, t.MarkdownQuote, t.MarkdownQuote
	t.HorizontalRule, t.HR, t.ListBullet = t.MarkdownRule, t.MarkdownRule, t.MarkdownBullet
	return t
}

type themeColors struct {
	text, muted, accent, border                 string
	mdHeading, mdLink, mdURL, mdCode            string
	mdCodeBlock, mdQuote, mdRule, mdBullet      string
	userBG, userText                            string
	toolPendingBG, toolSuccessBG, toolErrorBG   string
	toolTitle, toolOutput, notice, err          string
	composer, footer                            string
	thinkingOff, thinkingMinimal, thinkingLow   string
	thinkingMedium, thinkingHigh, thinkingXHigh string
}

var builtinThemes = map[string]Theme{
	// Values mirror Pi's packages/coding-agent dark.json. Empty foregrounds are
	// intentional: they retain the terminal's default text color. No theme sets
	// a page background.
	"dark": semanticTheme(themeColors{
		text: "", muted: fg("#808080"), accent: fg("#8abeb7"), border: fg("#5f87ff"),
		mdHeading: fg("#f0c674"), mdLink: fg("#81a2be"), mdURL: fg("#666666"), mdCode: fg("#de935f"),
		mdCodeBlock: fg("#b5bd68"), mdQuote: fg("#666666"), mdRule: fg("#666666"), mdBullet: fg("#8abeb7"),
		userBG: bg("#343541"), userText: "",
		toolPendingBG: bg("#282832"), toolSuccessBG: bg("#283228"), toolErrorBG: bg("#3c2828"),
		toolTitle: "", toolOutput: fg("#808080"), notice: fg("#ffff00"), err: fg("#cc6666"),
		composer: fg("#505050"), footer: fg("#666666"),
		thinkingOff: fg("#505050"), thinkingMinimal: fg("#6e6e6e"), thinkingLow: fg("#5f87af"),
		thinkingMedium: fg("#81a2be"), thinkingHigh: fg("#b294bb"), thinkingXHigh: fg("#d183e8"),
	}),
	"dracula": semanticTheme(themeColors{
		text: fg("#f8f8f2"), muted: fg("#6272a4"), accent: fg("#8be9fd"), border: fg("#6272a4"),
		mdHeading: fg("#f1fa8c"), mdLink: fg("#8be9fd"), mdURL: fg("#6272a4"), mdCode: fg("#ffb86c"),
		mdCodeBlock: fg("#50fa7b"), mdQuote: fg("#6272a4"), mdRule: fg("#6272a4"), mdBullet: fg("#bd93f9"),
		userBG: bg("#44475a"), userText: fg("#f8f8f2"),
		toolPendingBG: bg("#44475a"), toolSuccessBG: bg("#31483b"), toolErrorBG: bg("#49343f"),
		toolTitle: fg("#f8f8f2"), toolOutput: fg("#6272a4"), notice: fg("#f1fa8c"), err: fg("#ff5555"),
		composer: fg("#6272a4"), footer: fg("#6272a4"),
		thinkingOff: fg("#6272a4"), thinkingMinimal: fg("#8be9fd"), thinkingLow: fg("#50fa7b"),
		thinkingMedium: fg("#f1fa8c"), thinkingHigh: fg("#ff79c6"), thinkingXHigh: fg("#bd93f9"),
	}),
	"catppuccin-mocha": semanticTheme(themeColors{
		text: fg("#cdd6f4"), muted: fg("#6c7086"), accent: fg("#89b4fa"), border: fg("#45475a"),
		mdHeading: fg("#f9e2af"), mdLink: fg("#89b4fa"), mdURL: fg("#6c7086"), mdCode: fg("#fab387"),
		mdCodeBlock: fg("#a6e3a1"), mdQuote: fg("#6c7086"), mdRule: fg("#6c7086"), mdBullet: fg("#cba6f7"),
		userBG: bg("#313244"), userText: fg("#cdd6f4"),
		toolPendingBG: bg("#181825"), toolSuccessBG: bg("#1e2e2a"), toolErrorBG: bg("#30212b"),
		toolTitle: fg("#cdd6f4"), toolOutput: fg("#a6adc8"), notice: fg("#f9e2af"), err: fg("#f38ba8"),
		composer: fg("#45475a"), footer: fg("#6c7086"),
		thinkingOff: fg("#45475a"), thinkingMinimal: fg("#6c7086"), thinkingLow: fg("#89b4fa"),
		thinkingMedium: fg("#94e2d5"), thinkingHigh: fg("#cba6f7"), thinkingXHigh: fg("#f5c2e7"),
	}),
}
