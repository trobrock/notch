package tui

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// TranscriptKind identifies the visual treatment of a transcript entry.
type TranscriptKind string

const (
	TranscriptUser      TranscriptKind = "user"
	TranscriptAssistant TranscriptKind = "assistant"
	TranscriptThinking  TranscriptKind = "thinking"
	TranscriptTool      TranscriptKind = "tool"
	TranscriptNotice    TranscriptKind = "notice"
	TranscriptError     TranscriptKind = "error"
	TranscriptPrompt    TranscriptKind = "prompt"

	KindUser       = TranscriptUser
	KindAssistant  = TranscriptAssistant
	KindThinking   = TranscriptThinking
	KindTool       = TranscriptTool
	KindNotice     = TranscriptNotice
	KindError      = TranscriptError
	KindPrompt     = TranscriptPrompt
	EntryUser      = TranscriptUser
	EntryAssistant = TranscriptAssistant
	EntryThinking  = TranscriptThinking
	EntryTool      = TranscriptTool
	EntryNotice    = TranscriptNotice
	EntryError     = TranscriptError
	EntryPrompt    = TranscriptPrompt
)

// TranscriptEntry is one renderable item. Wrapping is cached on the entry so a
// streaming frame only re-wraps entries whose text or available width changed.
type TranscriptEntry struct {
	Kind    TranscriptKind
	Text    string
	Label   string
	Detail  string
	Pending bool
	Error   bool
	IsError bool

	// cacheLines is retained for the inexpensive plain wrapper used by tools
	// and notices. Markdown has an additional style key because the generated
	// ANSI must be rebuilt when either the base card style or theme changes.
	cacheWidth int
	cacheText  string
	cacheLines []string
	cacheMode  string
	cacheStyle string // diagnostic/style fingerprint, updated only on cache misses
	cacheBase  string
	cacheTheme Theme
}

// PendingMessage is one queued steering or follow-up message.
type PendingMessage struct {
	ID   string
	Mode string
	Text string
}

// CommandSuggestion is one slash-command completion row.
type CommandSuggestion struct {
	Name         string
	ArgumentHint string
	Description  string
}

// ExtensionPanel is keyed, non-interactive content published by an extension.
type ExtensionPanel struct {
	Key   string
	Title string
	Lines []string
}

// LayoutState is all of the data needed to build one terminal frame. Existing
// fields are retained; the semantic footer/theme fields can be populated by
// integrations as that information becomes available.
type LayoutState struct {
	Width, Height int
	Transcript    []TranscriptEntry
	Editor        *Editor
	ScrollOffset  int
	Provider      string
	Model         string
	Status        string
	Statuses      map[string]string
	Panels        map[string]ExtensionPanel
	Session       string
	Usage         string
	Theme         Theme

	CWD                string
	GitBranch          string
	ContextTokens      int
	ContextWindow      int
	AutoCompact        bool
	ThinkingLevel      string
	ThinkingFrame      int
	ThemeName          string
	CommandSuggestions []CommandSuggestion
	CommandSelection   int
	PendingMessages    []PendingMessage
}

// BuildFrame performs no terminal I/O. Every returned row has exactly the
// requested display width (ANSI escape sequences do not count toward it).
func BuildFrame(state *LayoutState) Frame {
	if state == nil || state.Width <= 0 || state.Height <= 0 {
		return Frame{}
	}
	width, height := state.Width, state.Height
	theme := completeTheme(state.Theme, state.ThemeName)
	text, cursor := editorState(state.Editor)

	composerLines, editorCursorRow, editorCursorCol := wrapEditor(text, cursor, max(1, width-1))
	if len(composerLines) == 0 {
		composerLines = []string{""}
	}
	const maxComposerLines = 8
	firstComposerLine := 0
	composerWanted := min(len(composerLines), maxComposerLines)
	if len(composerLines) > composerWanted {
		firstComposerLine = editorCursorRow - composerWanted + 1
		firstComposerLine = clamp(firstComposerLine, 0, len(composerLines)-composerWanted)
	}

	// Footer rows have first claim on tiny terminals. With fewer than three rows
	// above them, omit both borders rather than drawing a misleading half-box.
	footerCount := min(2, height)
	available := height - footerCount
	showBorders := available >= 3
	borderRows := 0
	if showBorders {
		borderRows = 2
	}
	composerCapacity := max(0, available-borderRows)
	composerCount := min(composerWanted, composerCapacity)
	if composerCount < composerWanted {
		firstComposerLine = clamp(editorCursorRow-composerCount+1, 0, len(composerLines)-composerCount)
	}
	visibleComposer := composerLines[firstComposerLine : firstComposerLine+composerCount]
	menuCapacity := max(0, height-footerCount-borderRows-composerCount)
	pendingCount := min(min(4, len(state.PendingMessages)), menuCapacity)
	menuCapacity -= pendingCount
	menuCount := min(min(8, len(state.CommandSuggestions)), menuCapacity)
	menuStart := 0
	if menuCount > 0 {
		selection := clamp(state.CommandSelection, 0, len(state.CommandSuggestions)-1)
		if selection >= menuCount {
			menuStart = selection - menuCount + 1
		}
		menuStart = clamp(menuStart, 0, len(state.CommandSuggestions)-menuCount)
	}
	transcriptHeight := height - footerCount - borderRows - composerCount - pendingCount - menuCount
	panelLines := renderPanels(state.Panels, width, theme)
	panelCount := min(len(panelLines), max(0, transcriptHeight/2))
	transcriptHeight -= panelCount

	frame := Frame{Rows: make([]string, height)}
	for i := range frame.Rows {
		frame.Rows[i] = strings.Repeat(" ", width)
	}

	transcript := renderTranscript(state, width, theme)
	start := len(transcript) - transcriptHeight - max(0, state.ScrollOffset)
	if start < 0 {
		start = 0
	}
	end := min(len(transcript), start+transcriptHeight)
	for i, line := range transcript[start:end] {
		// Unlike the old layout, short transcripts start at the top of their
		// viewport, leaving breathing room above the fixed composer.
		frame.Rows[i] = padANSI(line, width)
	}
	if transcriptHeight > 0 && state.ScrollOffset > 0 && len(transcript) > transcriptHeight {
		indicator := fmt.Sprintf("↑ %d", state.ScrollOffset)
		frame.Rows[0] = overlayRight(frame.Rows[0], theme.Notice+indicator+theme.Reset, width)
	}
	for i := 0; i < panelCount; i++ {
		frame.Rows[transcriptHeight+i] = padANSI(panelLines[i], width)
	}

	row := transcriptHeight + panelCount
	pendingStart := max(0, len(state.PendingMessages)-pendingCount)
	for i := pendingStart; i < len(state.PendingMessages); i++ {
		message := state.PendingMessages[i]
		label, marker := "Steering", "↪"
		if message.Mode == "follow_up" {
			label, marker = "Follow-up", "↳"
		}
		frame.Rows[row] = styleFull(theme.Muted+"\x1b[3m", fmt.Sprintf(" %s %s queued: %s", marker, label, sanitiseTerminalText(message.Text)), width, theme.Reset)
		row++
	}
	for i := 0; i < menuCount; i++ {
		index := menuStart + i
		suggestion := state.CommandSuggestions[index]
		marker, style := "  ", theme.Muted
		if index == clamp(state.CommandSelection, 0, len(state.CommandSuggestions)-1) {
			marker, style = "› ", theme.Accent+"\x1b[1m"
		}
		command := "/" + suggestion.Name
		if suggestion.ArgumentHint != "" {
			command += " " + suggestion.ArgumentHint
		}
		line := marker + command
		if suggestion.Description != "" {
			line += " — " + suggestion.Description
		}
		frame.Rows[row] = styleFull(style, line, width, theme.Reset)
		row++
	}
	borderStyle := thinkingBorderStyle(theme, state.ThinkingLevel)
	if showBorders {
		frame.Rows[row] = styleFull(borderStyle, strings.Repeat("─", width), width, theme.Reset)
		row++
	}
	composerTop := row
	for i, line := range visibleComposer {
		frame.Rows[row+i] = padANSI(theme.Text+" "+line+theme.Reset, width)
	}
	row += composerCount
	if showBorders {
		frame.Rows[row] = styleFull(borderStyle, strings.Repeat("─", width), width, theme.Reset)
		row++
	}

	footer := footerText(state, width)
	footerStart := 2 - footerCount
	for i := 0; i < footerCount; i++ {
		frame.Rows[row+i] = styleFull(theme.Footer, footer[footerStart+i], width, theme.Reset)
	}

	cursorScreenRow := composerTop + (editorCursorRow - firstComposerLine)
	cursorScreenCol := min(width-1, max(0, 1+editorCursorCol))
	frame.CursorVisible = composerCount > 0 && cursorScreenRow >= composerTop && cursorScreenRow < composerTop+composerCount
	if frame.CursorVisible {
		frame.CursorRow, frame.CursorCol = cursorScreenRow, cursorScreenCol
	} else {
		frame.CursorRow, frame.CursorCol = height-1, 0
	}
	return frame
}

func completeTheme(t Theme, name string) Theme {
	base, ok := ThemeByName(name)
	if !ok {
		base = DefaultTheme()
	}
	// Translate old partial themes before filling semantic fields.
	if t.Text == "" && t.Assistant != "" {
		t.Text = t.Assistant
	}
	if t.UserText == "" && t.User != "" {
		t.UserText = t.User
	}
	if t.ToolTitle == "" && t.Tool != "" {
		t.ToolTitle = t.Tool
	}
	if t.ToolOutput == "" && t.Pending != "" {
		t.ToolOutput = t.Pending
	}
	if t.Footer == "" && t.Status != "" {
		t.Footer = t.Status
	}

	fill := func(dst *string, fallback string) {
		if *dst == "" {
			*dst = fallback
		}
	}
	fill(&t.Text, base.Text)
	fill(&t.Muted, base.Muted)
	fill(&t.Accent, base.Accent)
	fill(&t.Border, base.Border)
	// Accept both spellings when callers construct a custom semantic theme.
	if t.MarkdownHeading == "" {
		t.MarkdownHeading = t.Heading
	}
	if t.MarkdownLink == "" {
		t.MarkdownLink = t.Link
	}
	if t.MarkdownURL == "" {
		t.MarkdownURL = t.LinkURL
		if t.MarkdownURL == "" {
			t.MarkdownURL = t.LinkUrl
		}
	}
	if t.MarkdownCode == "" {
		t.MarkdownCode = t.InlineCode
		if t.MarkdownCode == "" {
			t.MarkdownCode = t.Code
		}
	}
	if t.MarkdownCodeBlock == "" {
		t.MarkdownCodeBlock = t.CodeBlock
	}
	if t.MarkdownQuote == "" {
		t.MarkdownQuote = t.BlockQuote
		if t.MarkdownQuote == "" {
			t.MarkdownQuote = t.Quote
		}
		if t.MarkdownQuote == "" {
			t.MarkdownQuote = t.QuoteBorder
		}
	}
	if t.MarkdownRule == "" {
		t.MarkdownRule = t.HorizontalRule
		if t.MarkdownRule == "" {
			t.MarkdownRule = t.HR
		}
	}
	if t.MarkdownBullet == "" {
		t.MarkdownBullet = t.ListBullet
	}
	fill(&t.MarkdownHeading, base.MarkdownHeading)
	fill(&t.MarkdownLink, base.MarkdownLink)
	fill(&t.MarkdownURL, base.MarkdownURL)
	fill(&t.MarkdownCode, base.MarkdownCode)
	fill(&t.MarkdownCodeBlock, base.MarkdownCodeBlock)
	fill(&t.MarkdownQuote, base.MarkdownQuote)
	fill(&t.MarkdownRule, base.MarkdownRule)
	fill(&t.MarkdownBullet, base.MarkdownBullet)
	fill(&t.UserBG, base.UserBG)
	fill(&t.UserText, base.UserText)
	fill(&t.ToolPendingBG, base.ToolPendingBG)
	fill(&t.ToolSuccessBG, base.ToolSuccessBG)
	fill(&t.ToolErrorBG, base.ToolErrorBG)
	fill(&t.ToolTitle, base.ToolTitle)
	fill(&t.ToolOutput, base.ToolOutput)
	fill(&t.Notice, base.Notice)
	fill(&t.Error, base.Error)
	fill(&t.Composer, base.Composer)
	fill(&t.Footer, base.Footer)
	fill(&t.ThinkingOff, base.ThinkingOff)
	fill(&t.ThinkingMinimal, base.ThinkingMinimal)
	fill(&t.ThinkingLow, base.ThinkingLow)
	fill(&t.ThinkingMedium, base.ThinkingMedium)
	fill(&t.ThinkingHigh, base.ThinkingHigh)
	fill(&t.ThinkingXHigh, base.ThinkingXHigh)
	fill(&t.Reset, base.Reset)
	fill(&t.User, t.UserText)
	fill(&t.Assistant, t.Text)
	fill(&t.Tool, t.ToolTitle)
	fill(&t.Pending, t.Muted)
	fill(&t.Status, t.Footer)
	fill(&t.Heading, t.MarkdownHeading)
	fill(&t.Link, t.MarkdownLink)
	fill(&t.LinkURL, t.MarkdownURL)
	fill(&t.LinkUrl, t.MarkdownURL)
	fill(&t.InlineCode, t.MarkdownCode)
	fill(&t.Code, t.MarkdownCode)
	fill(&t.CodeBlock, t.MarkdownCodeBlock)
	fill(&t.CodeBlockBorder, t.MarkdownQuote)
	fill(&t.BlockQuote, t.MarkdownQuote)
	fill(&t.Quote, t.MarkdownQuote)
	fill(&t.QuoteBorder, t.MarkdownQuote)
	fill(&t.HorizontalRule, t.MarkdownRule)
	fill(&t.HR, t.MarkdownRule)
	fill(&t.ListBullet, t.MarkdownBullet)
	return t
}

func thinkingBorderStyle(theme Theme, level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "off":
		return theme.ThinkingOff
	case "minimal", "min":
		return theme.ThinkingMinimal
	case "low":
		return theme.ThinkingLow
	case "medium", "med":
		return theme.ThinkingMedium
	case "high":
		return theme.ThinkingHigh
	case "xhigh", "x-high", "extra-high":
		return theme.ThinkingXHigh
	default:
		if theme.Composer != "" {
			return theme.Composer
		}
		return theme.Border
	}
}

func renderTranscript(state *LayoutState, width int, theme Theme) []string {
	var result []string
	blank := strings.Repeat(" ", width)
	for i := range state.Transcript {
		entry := &state.Transcript[i]
		isError := entry.Error || entry.IsError || entry.Kind == TranscriptError
		switch entry.Kind {
		case TranscriptUser:
			style := theme.UserBG + theme.UserText
			lines := entry.markdownPreserveLines(max(1, width-2), entry.Text, theme, style)
			result = append(result, styleFull(style, blank, width, theme.Reset))
			if entry.Label == "steer" || entry.Label == "follow_up" {
				label := "Steering"
				if entry.Label == "follow_up" {
					label = "Follow-up"
				}
				result = append(result, styleFull(style, " "+theme.Muted+"\x1b[3m"+label+theme.Reset+style, width, theme.Reset))
			}
			for _, line := range lines {
				result = append(result, styleFull(style, " "+line, width, theme.Reset))
			}
			result = append(result, styleFull(style, blank, width, theme.Reset))

		case TranscriptThinking:
			style := theme.Muted + "\x1b[3m"
			if entry.Pending && strings.TrimSpace(entry.Text) == "" {
				spinner := []string{"◐", "◓", "◑", "◒"}
				frame := state.ThinkingFrame % len(spinner)
				if frame < 0 {
					frame += len(spinner)
				}
				result = append(result, styleFull(style, " "+spinner[frame]+" Thinking…", width, theme.Reset))
				break
			}
			result = append(result, styleFull(style, " ◆ Thinking", width, theme.Reset))
			for _, line := range entry.markdown(max(1, width-2), entry.Text, theme, style) {
				result = append(result, padANSI("  "+line, width))
			}

		case TranscriptTool:
			bgStyle, icon := theme.ToolSuccessBG, "✓"
			if entry.Pending {
				bgStyle, icon = theme.ToolPendingBG, "●"
			}
			if isError {
				bgStyle, icon = theme.ToolErrorBG, "✗"
			}
			label := sanitiseTerminalText(strings.TrimSpace(entry.Label))
			if label == "" {
				label = "tool"
			}
			detail := sanitiseTerminalText(strings.TrimSpace(entry.Detail))
			result = append(result, styleFull(bgStyle, blank, width, theme.Reset))
			titlePlain := " " + icon + " " + label
			title := " " + icon + " \x1b[1m" + label + "\x1b[22m"
			if detail != "" && visibleWidth(titlePlain)+1+runewidth.StringWidth(detail) <= width {
				title += " " + theme.Muted + detail + theme.Reset + bgStyle + theme.ToolTitle
				detail = ""
			}
			result = append(result, styleFull(bgStyle+theme.ToolTitle, title, width, theme.Reset))
			if detail != "" {
				for _, line := range wrapWords(detail, max(1, width-3)) {
					result = append(result, styleFull(bgStyle+theme.Muted, "   "+line, width, theme.Reset))
				}
			}
			if entry.Text != "" {
				for _, line := range entry.wrapped(max(1, width-4), entry.Text) {
					content := " " + theme.MarkdownQuote + "│ " + theme.Reset + bgStyle + theme.ToolOutput + line
					result = append(result, styleFull(bgStyle+theme.ToolOutput, content, width, theme.Reset))
				}
			}
			result = append(result, styleFull(bgStyle, blank, width, theme.Reset))

		case TranscriptPrompt:
			style := theme.Text
			lines := strings.Split(entry.Text, "\n")
			for _, line := range lines {
				line = sanitiseTerminalText(line)
				switch {
				case strings.HasPrefix(line, "? "):
					result = append(result, styleFull(style+"\x1b[1m", " "+line, width, theme.Reset))
				case strings.HasPrefix(line, "❯ "):
					result = append(result, styleFull(theme.Accent+"\x1b[1m", " "+line, width, theme.Reset))
				case strings.HasPrefix(line, "  "):
					result = append(result, styleFull(theme.Muted, " "+line, width, theme.Reset))
				default:
					result = append(result, styleFull(style, " "+line, width, theme.Reset))
				}
			}

		case TranscriptNotice, TranscriptError:
			style, label := theme.Notice, entry.Label
			if isError {
				style = theme.Error
				if label == "" {
					label = "error"
				}
			}
			text := entry.Text
			if entry.Pending {
				text = pendingText(text)
			}
			if label != "" {
				text = label + ": " + text
			}
			for _, line := range entry.wrapped(max(1, width-1), text) {
				result = append(result, padANSI(style+" "+line+theme.Reset, width))
			}

		default:
			text := entry.Text
			if entry.Pending {
				text = pendingText(text)
			}
			if entry.Label != "" {
				text = entry.Label + ": " + text
			}
			for _, line := range entry.markdown(max(1, width-1), text, theme, theme.Text) {
				result = append(result, styleFull(theme.Text, " "+line, width, theme.Reset))
			}
		}
		if i+1 < len(state.Transcript) {
			result = append(result, blank)
		}
	}
	return result
}

func pendingText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "…"
	}
	return text + " …"
}

func (entry *TranscriptEntry) wrapped(width int, text string) []string {
	if entry.cacheMode == "plain" && entry.cacheWidth == width && entry.cacheText == text && entry.cacheLines != nil {
		return entry.cacheLines
	}
	entry.cacheMode = "plain"
	entry.cacheWidth = width
	entry.cacheText = text
	entry.cacheStyle = ""
	entry.cacheBase = ""
	entry.cacheTheme = Theme{}
	entry.cacheLines = wrapWords(text, width)
	return entry.cacheLines
}

func (entry *TranscriptEntry) markdown(width int, text string, theme Theme, base string) []string {
	if entry.cacheMode == "markdown" && entry.cacheWidth == width && entry.cacheText == text && entry.cacheBase == base && entry.cacheTheme == theme && entry.cacheLines != nil {
		return entry.cacheLines
	}
	entry.cacheMode = "markdown"
	entry.cacheWidth = width
	entry.cacheText = text
	entry.cacheBase = base
	entry.cacheTheme = theme
	entry.cacheStyle = markdownThemeKey(theme, base)
	entry.cacheLines = renderMarkdown(text, width, theme, base)
	return entry.cacheLines
}

func (entry *TranscriptEntry) markdownPreserveLines(width int, text string, theme Theme, base string) []string {
	mode := "markdown-preserve-lines"
	if entry.cacheMode == mode && entry.cacheWidth == width && entry.cacheText == text && entry.cacheBase == base && entry.cacheTheme == theme && entry.cacheLines != nil {
		return entry.cacheLines
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	parts := strings.Split(text, "\n")
	lines := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, renderMarkdown(part, width, theme, base)...)
	}
	entry.cacheMode, entry.cacheWidth, entry.cacheText = mode, width, text
	entry.cacheBase, entry.cacheTheme, entry.cacheStyle = base, theme, markdownThemeKey(theme, base)
	entry.cacheLines = lines
	return lines
}

func markdownThemeKey(t Theme, base string) string {
	return strings.Join([]string{base, t.Reset, t.MarkdownHeading, t.MarkdownLink, t.MarkdownURL,
		t.MarkdownCode, t.MarkdownCodeBlock, t.MarkdownQuote, t.MarkdownRule, t.MarkdownBullet}, "\x00")
}

func wrapWords(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	text = sanitiseTerminalText(text)
	paragraphs := strings.Split(text, "\n")
	lines := make([]string, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}
		current := ""
		currentWidth := 0
		for _, word := range words {
			pieces := splitDisplay(word, width)
			for p, piece := range pieces {
				pieceWidth := runewidth.StringWidth(piece)
				space := 0
				if current != "" && p == 0 {
					space = 1
				}
				if current != "" && currentWidth+space+pieceWidth <= width {
					if space != 0 {
						current += " "
						currentWidth++
					}
					current += piece
					currentWidth += pieceWidth
					continue
				}
				if current != "" {
					lines = append(lines, current)
				}
				current, currentWidth = piece, pieceWidth
				if p < len(pieces)-1 {
					lines = append(lines, current)
					current, currentWidth = "", 0
				}
			}
		}
		if current != "" {
			lines = append(lines, current)
		}
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

func splitDisplay(s string, width int) []string {
	if runewidth.StringWidth(s) <= width {
		return []string{s}
	}
	var pieces []string
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := runeWidth(r)
		if used > 0 && used+rw > width {
			pieces = append(pieces, b.String())
			b.Reset()
			used = 0
		}
		b.WriteRune(printableRune(r))
		used += rw
	}
	if b.Len() > 0 {
		pieces = append(pieces, b.String())
	}
	return pieces
}

func wrapEditor(text string, cursor, width int) ([]string, int, int) {
	if width < 1 {
		width = 1
	}
	runeCount := utf8.RuneCountInString(text)
	if cursor < 0 {
		cursor = 0
	}
	if cursor > runeCount {
		cursor = runeCount
	}
	lines := []string{""}
	line, col, runeIndex := 0, 0, 0
	cursorLine, cursorCol := 0, 0
	cursorSet := false
	for _, original := range text {
		r := original
		if r != '\n' && r != '\r' {
			r = printableRune(r)
			rw := runeWidth(r)
			if col > 0 && col+rw > width {
				lines = append(lines, "")
				line++
				col = 0
			}
		}
		if !cursorSet && runeIndex == cursor {
			cursorLine, cursorCol, cursorSet = line, col, true
		}
		runeIndex++
		if original == '\r' {
			continue
		}
		if original == '\n' {
			lines = append(lines, "")
			line++
			col = 0
			continue
		}
		lines[line] += string(r)
		col += runeWidth(r)
	}
	if !cursorSet {
		cursorLine, cursorCol = line, col
		// A cursor after a completely full soft-wrapped line belongs at the
		// beginning of the following visual line.
		if cursorCol >= width {
			lines = append(lines, "")
			cursorLine++
			cursorCol = 0
		}
	}
	return lines, cursorLine, cursorCol
}

func editorState(editor *Editor) (string, int) {
	if editor == nil {
		return "", 0
	}
	return editor.Text(), editor.Cursor()
}

func renderPanels(panels map[string]ExtensionPanel, width int, theme Theme) []string {
	if len(panels) == 0 || width < 4 {
		return nil
	}
	keys := make([]string, 0, len(panels))
	for key := range panels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []string
	for _, key := range keys {
		panel := panels[key]
		if strings.TrimSpace(panel.Title) == "" && len(panel.Lines) == 0 {
			continue
		}
		title := strings.TrimSpace(panel.Title)
		if title == "" {
			title = key
		}
		out = append(out, theme.Notice+"── "+title+" "+strings.Repeat("─", max(0, width-visibleWidth(title)-4))+theme.Reset)
		for _, line := range panel.Lines {
			out = append(out, "  "+line)
		}
	}
	return out
}

func footerText(state *LayoutState, width int) [2]string {
	pwd := strings.TrimSpace(state.CWD)
	if state.GitBranch != "" {
		if pwd == "" {
			pwd = "(" + state.GitBranch + ")"
		} else {
			pwd += " (" + state.GitBranch + ")"
		}
	}

	leftParts := make([]string, 0, 4)
	for _, value := range []string{state.Status, state.Session, state.Usage} {
		if value = strings.TrimSpace(value); value != "" && value != "ready" {
			leftParts = append(leftParts, value)
		}
	}
	statusKeys := make([]string, 0, len(state.Statuses))
	for key := range state.Statuses {
		statusKeys = append(statusKeys, key)
	}
	sort.Strings(statusKeys)
	for _, key := range statusKeys {
		if value := strings.TrimSpace(state.Statuses[key]); value != "" {
			leftParts = append(leftParts, value)
		}
	}
	if state.ContextWindow > 0 {
		context := "?"
		if state.ContextTokens >= 0 {
			context = fmt.Sprintf("%.1f%%", 100*float64(state.ContextTokens)/float64(state.ContextWindow))
		}
		context += "/" + formatTokens(state.ContextWindow)
		if state.AutoCompact {
			context += " (auto)"
		}
		leftParts = append(leftParts, context)
	}
	left := strings.Join(leftParts, " ")

	right := strings.TrimSpace(state.Model)
	if provider := strings.TrimSpace(state.Provider); provider != "" {
		model := right
		right = "(" + provider + ")"
		if model != "" {
			right += " " + model
		}
	}
	level := strings.ToLower(strings.TrimSpace(state.ThinkingLevel))
	if level == "" || level == "off" {
		level = "thinking off"
	}
	if right == "" {
		right = level
	} else {
		right += " • " + level
	}
	return [2]string{truncateANSI(pwd, width), alignFooter(left, right, width)}
}

func alignFooter(left, right string, width int) string {
	left = truncateANSI(left, width)
	leftWidth := visibleWidth(left)
	if right == "" || leftWidth >= width-1 {
		return padANSI(left, width)
	}
	availableRight := width - leftWidth - 2
	if availableRight <= 0 {
		return padANSI(left, width)
	}
	right = truncateANSI(right, availableRight)
	return left + strings.Repeat(" ", width-leftWidth-visibleWidth(right)) + right
}

func formatTokens(count int) string {
	switch {
	case count < 1000:
		return fmt.Sprintf("%d", count)
	case count < 10000:
		return fmt.Sprintf("%.1fk", float64(count)/1000)
	case count < 1000000:
		return fmt.Sprintf("%dk", int(float64(count)/1000+0.5))
	case count < 10000000:
		return fmt.Sprintf("%.1fM", float64(count)/1000000)
	default:
		return fmt.Sprintf("%dM", int(float64(count)/1000000+0.5))
	}
}

func styleFull(style, text string, width int, reset string) string {
	text = truncateANSI(text, width)
	padding := strings.Repeat(" ", max(0, width-visibleWidth(text)))
	if style == "" {
		return text + padding
	}
	return style + text + padding + reset
}

func padANSI(text string, width int) string {
	text = truncateANSI(text, width)
	return text + strings.Repeat(" ", max(0, width-visibleWidth(text)))
}

func overlayRight(base, overlay string, width int) string {
	overlayWidth := visibleWidth(overlay)
	if overlayWidth >= width {
		return padANSI(overlay, width)
	}
	left := truncateANSI(base, width-overlayWidth)
	left = padANSI(left, width-overlayWidth)
	return left + overlay
}

func visibleWidth(s string) int {
	width := 0
	for len(s) > 0 {
		if n := ansiSequenceLen(s); n > 0 {
			s = s[n:]
			continue
		}
		r, n := utf8.DecodeRuneInString(s)
		if n == 0 {
			break
		}
		width += runeWidth(r)
		s = s[n:]
	}
	return width
}

func truncateANSI(s string, width int) string {
	if width <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	for len(s) > 0 {
		if n := ansiSequenceLen(s); n > 0 {
			b.WriteString(s[:n])
			s = s[n:]
			continue
		}
		r, n := utf8.DecodeRuneInString(s)
		rw := runeWidth(r)
		if used+rw > width {
			break
		}
		b.WriteString(s[:n])
		used += rw
		s = s[n:]
	}
	return b.String()
}

func ansiSequenceLen(s string) int {
	if len(s) < 2 || s[0] != 0x1b {
		return 0
	}
	if s[1] == '[' {
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return len(s)
	}
	if s[1] == ']' {
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return len(s)
	}
	return 2
}

func runeWidth(r rune) int {
	if r == '\t' {
		return 1
	}
	width := runewidth.RuneWidth(r)
	if width < 0 {
		return 0
	}
	return width
}

func printableRune(r rune) rune {
	if r == '\t' {
		return ' '
	}
	if unicode.IsControl(r) {
		return ' '
	}
	return r
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
