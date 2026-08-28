package tui

// This file deliberately renders goldmark's AST rather than converting Markdown
// to HTML and trying to interpret the result.  Apart from being considerably
// cheaper, that keeps terminal control characters in the input as ordinary
// (sanitised) text.

import (
	"fmt"
	"html"
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	textm "github.com/yuin/goldmark/text"
)

type markdownSpan struct {
	text     string
	style    string
	preserve bool // structural leading space (for nested list continuation)
}

type markdownLine struct {
	spans    []markdownSpan
	verbatim bool
	rule     bool
}

type markdownRenderer struct {
	source []byte
	theme  Theme
	width  int
}

var markdownParser = goldmark.New(goldmark.WithExtensions(extension.Table))

// renderMarkdown returns unpadded ANSI lines. width is a display-cell width,
// not a byte count. base is restored after every inline style reset (important
// for a user card, whose background must continue through bold/code/link text).
func renderMarkdown(source string, width int, theme Theme, base string) []string {
	if width < 1 {
		width = 1
	}
	data := []byte(strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\r", "\n"))
	doc := markdownParser.Parser().Parse(textm.NewReader(data))
	r := markdownRenderer{source: data, theme: theme, width: width}
	logical := r.blocks(doc)
	if len(logical) == 0 {
		logical = []markdownLine{{}}
	}

	var out []string
	for _, line := range logical {
		wrapped := wrapMarkdownLine(line, width)
		for _, part := range wrapped {
			out = append(out, styledMarkdownLine(part, base, theme.Reset))
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// RenderMarkdown is useful to small integrations which want the same Markdown
// dialect as the transcript without constructing a complete frame.
func RenderMarkdown(source string, width int, theme Theme) []string {
	theme = completeTheme(theme, "")
	return renderMarkdown(source, width, theme, theme.Text)
}

func (r markdownRenderer) blocks(parent ast.Node) []markdownLine {
	var out []markdownLine
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		var block []markdownLine
		switch n := n.(type) {
		case *ast.Heading:
			style := r.theme.MarkdownHeading + "\x1b[1m"
			block = []markdownLine{{spans: r.inlines(n, style)}}
		case *ast.Paragraph, *ast.TextBlock:
			block = []markdownLine{{spans: r.inlines(n, "")}}
		case *ast.FencedCodeBlock:
			block = r.codeBlock(n.Lines())
		case *ast.CodeBlock:
			block = r.codeBlock(n.Lines())
		case *ast.Blockquote:
			block = r.blocks(n)
			if len(block) == 0 {
				block = []markdownLine{{}}
			}
			for i := range block {
				block[i].spans = append([]markdownSpan{{text: "│ ", style: r.theme.MarkdownQuote, preserve: true}}, block[i].spans...)
			}
		case *ast.ThematicBreak:
			block = []markdownLine{{spans: []markdownSpan{{text: "─", style: r.theme.MarkdownRule}}, rule: true}}
		case *ast.List:
			block = r.list(n)
		case *extast.Table:
			block = r.table(n)
		case *ast.HTMLBlock:
			// Raw HTML has no terminal semantics.  Showing it literally is safer
			// and less surprising than silently dropping streamed model output.
			block = r.code(n.Lines())
		default:
			if n.HasChildren() {
				block = r.blocks(n)
			}
		}
		out = appendMarkdownBlock(out, block)
	}
	return trimMarkdownBlanks(out)
}

func appendMarkdownBlock(dst, block []markdownLine) []markdownLine {
	block = trimMarkdownBlanks(block)
	if len(block) == 0 {
		return dst
	}
	// Markdown block separation is exactly one row, irrespective of how many
	// blank source rows goldmark consumed.
	if len(dst) > 0 && !markdownLineBlank(dst[len(dst)-1]) {
		dst = append(dst, markdownLine{})
	}
	return append(dst, block...)
}

func trimMarkdownBlanks(lines []markdownLine) []markdownLine {
	for len(lines) > 0 && markdownLineBlank(lines[0]) {
		lines = lines[1:]
	}
	for len(lines) > 0 && markdownLineBlank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func markdownLineBlank(line markdownLine) bool {
	if line.verbatim || line.rule {
		return false
	}
	for _, s := range line.spans {
		if strings.TrimSpace(s.text) != "" {
			return false
		}
	}
	return true
}

func (r markdownRenderer) codeBlock(lines *textm.Segments) []markdownLine {
	block := r.code(lines)
	for i := range block {
		block[i].spans = append([]markdownSpan{{text: "│ ", style: r.theme.CodeBlockBorder, preserve: true}}, block[i].spans...)
	}
	return block
}

func (r markdownRenderer) code(lines *textm.Segments) []markdownLine {
	var out []markdownLine
	for i := 0; i < lines.Len(); i++ {
		segment := lines.At(i)
		value := string(segment.Value(r.source))
		value = strings.TrimSuffix(value, "\n")
		value = strings.TrimSuffix(value, "\r")
		// A fixed-width tab expansion makes wrapping deterministic while retaining
		// indentation (a literal tab has terminal-dependent width).
		value = strings.ReplaceAll(value, "\t", "    ")
		out = append(out, markdownLine{spans: []markdownSpan{{text: value, style: r.theme.MarkdownCodeBlock}}, verbatim: true})
	}
	if len(out) == 0 {
		out = append(out, markdownLine{spans: []markdownSpan{{style: r.theme.MarkdownCodeBlock}}, verbatim: true})
	}
	return out
}

func (r markdownRenderer) table(table *extast.Table) []markdownLine {
	type row struct {
		cells  [][]markdownSpan
		aligns []extast.Alignment
		header bool
	}

	var rows []row
	columns := 0
	for node := table.FirstChild(); node != nil; node = node.NextSibling() {
		current := row{}
		switch node.(type) {
		case *extast.TableHeader:
			current.header = true
		case *extast.TableRow:
		default:
			continue
		}
		for child := node.FirstChild(); child != nil; child = child.NextSibling() {
			cell, ok := child.(*extast.TableCell)
			if !ok {
				continue
			}
			style := ""
			if current.header {
				style = "\x1b[1m"
			}
			current.cells = append(current.cells, r.inlines(cell, style))
			current.aligns = append(current.aligns, cell.Alignment)
		}
		if len(current.cells) > columns {
			columns = len(current.cells)
		}
		rows = append(rows, current)
	}
	if columns == 0 {
		return nil
	}

	// A boxed table needs one content cell per column, two spaces around each
	// cell, and its vertical borders. At very narrow widths, retain the parsed
	// cells but fall back to ordinary wrapping instead of emitting broken boxes.
	minimumWidth := 4*columns + 1
	if r.width < minimumWidth {
		var out []markdownLine
		for _, row := range rows {
			var spans []markdownSpan
			for i, cell := range row.cells {
				if i > 0 {
					spans = appendSpan(spans, " │ ", r.theme.MarkdownRule)
				}
				for _, span := range cell {
					spans = appendSpan(spans, span.text, span.style)
				}
			}
			out = append(out, markdownLine{spans: spans})
		}
		return out
	}

	widths := make([]int, columns)
	for i := range widths {
		widths[i] = 1
	}
	for _, row := range rows {
		for column, cell := range row.cells {
			cellWidth := markdownSpansWidth(cell)
			if cellWidth > r.width {
				cellWidth = r.width
			}
			if cellWidth > widths[column] {
				widths[column] = cellWidth
			}
		}
	}
	budget := r.width - 3*columns - 1
	for totalInt(widths) > budget {
		widest := 0
		for i := 1; i < len(widths); i++ {
			if widths[i] > widths[widest] {
				widest = i
			}
		}
		if widths[widest] <= 1 {
			break
		}
		widths[widest]--
	}

	border := func(left, middle, right string) markdownLine {
		var b strings.Builder
		b.WriteString(left)
		for i, width := range widths {
			if i > 0 {
				b.WriteString(middle)
			}
			b.WriteString(strings.Repeat("─", width+2))
		}
		b.WriteString(right)
		return markdownLine{spans: []markdownSpan{{text: b.String(), style: r.theme.MarkdownRule}}, verbatim: true}
	}

	out := []markdownLine{border("┌", "┬", "┐")}
	for rowIndex, row := range rows {
		wrapped := make([][]markdownLine, columns)
		height := 1
		for column := 0; column < columns; column++ {
			var cell []markdownSpan
			if column < len(row.cells) {
				cell = row.cells[column]
			}
			wrapped[column] = wrapMarkdownLine(markdownLine{spans: cell}, widths[column])
			if len(wrapped[column]) > height {
				height = len(wrapped[column])
			}
		}
		for lineIndex := 0; lineIndex < height; lineIndex++ {
			line := markdownLine{verbatim: true}
			line.spans = appendSpan(line.spans, "│", r.theme.MarkdownRule)
			for column := 0; column < columns; column++ {
				line.spans = appendSpan(line.spans, " ", "")
				var content markdownLine
				if lineIndex < len(wrapped[column]) {
					content = wrapped[column][lineIndex]
				}
				contentWidth := markdownSpansWidth(content.spans)
				left, right := tablePadding(widths[column]-contentWidth, alignmentAt(row.aligns, column))
				line.spans = appendSpan(line.spans, strings.Repeat(" ", left), "")
				for _, span := range content.spans {
					line.spans = appendSpan(line.spans, span.text, span.style)
				}
				line.spans = appendSpan(line.spans, strings.Repeat(" ", right)+" ", "")
				line.spans = appendSpan(line.spans, "│", r.theme.MarkdownRule)
			}
			out = append(out, line)
		}
		if row.header && rowIndex < len(rows)-1 {
			out = append(out, border("├", "┼", "┤"))
		}
	}
	out = append(out, border("└", "┴", "┘"))
	return out
}

func markdownSpansWidth(spans []markdownSpan) int {
	width := 0
	for _, span := range spans {
		width += runewidth.StringWidth(span.text)
	}
	return width
}

func totalInt(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}

func alignmentAt(alignments []extast.Alignment, column int) extast.Alignment {
	if column < len(alignments) {
		return alignments[column]
	}
	return extast.AlignNone
}

func tablePadding(space int, alignment extast.Alignment) (left, right int) {
	if space < 0 {
		space = 0
	}
	switch alignment {
	case extast.AlignRight:
		return space, 0
	case extast.AlignCenter:
		return space / 2, space - space/2
	default:
		return 0, space
	}
}

func (r markdownRenderer) list(list *ast.List) []markdownLine {
	var out []markdownLine
	number := list.Start
	for itemNode := list.FirstChild(); itemNode != nil; itemNode = itemNode.NextSibling() {
		item, ok := itemNode.(*ast.ListItem)
		if !ok {
			continue
		}
		prefix := "• "
		if list.IsOrdered() {
			prefix = fmt.Sprintf("%d. ", number)
			number++
		}
		body := r.blocks(item)
		if len(body) == 0 {
			body = []markdownLine{{}}
		}
		indent := strings.Repeat(" ", runewidth.StringWidth(prefix))
		for i := range body {
			p := indent
			style := ""
			if i == 0 {
				p, style = prefix, r.theme.MarkdownBullet
			}
			body[i].spans = append([]markdownSpan{{text: p, style: style, preserve: true}}, body[i].spans...)
		}
		if list.IsTight {
			compact := body[:0]
			for _, line := range body {
				if !markdownLineBlank(line) {
					compact = append(compact, line)
				}
			}
			body = compact
		}
		if len(out) > 0 && !list.IsTight {
			out = append(out, markdownLine{})
		}
		out = append(out, body...)
	}
	return trimMarkdownBlanks(out)
}

func (r markdownRenderer) inlines(parent ast.Node, inherited string) []markdownSpan {
	var spans []markdownSpan
	var visit func(ast.Node, string)
	visit = func(parent ast.Node, style string) {
		for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
			switch n := n.(type) {
			case *ast.Text:
				value := string(n.Value(r.source))
				if !n.IsRaw() {
					value = markdownTextValue(value)
				}
				spans = appendSpan(spans, value, style)
				if n.HardLineBreak() {
					spans = appendSpan(spans, "\n", style)
				} else if n.SoftLineBreak() {
					spans = appendSpan(spans, " ", style)
				}
			case *ast.String:
				value := string(n.Value)
				if !n.IsRaw() && !n.IsCode() {
					value = markdownTextValue(value)
				}
				spans = appendSpan(spans, value, style)
			case *ast.Emphasis:
				emphasis := "\x1b[3m"
				if n.Level >= 2 {
					emphasis = "\x1b[1m"
				}
				visit(n, style+emphasis)
			case *ast.CodeSpan:
				visit(n, style+r.theme.MarkdownCode)
			case *ast.Link:
				visit(n, style+r.theme.MarkdownLink+"\x1b[4m")
				dest := sanitiseTerminalText(string(n.Destination))
				if dest != "" {
					spans = appendSpan(spans, " ("+dest+")", style+r.theme.MarkdownURL)
				}
			case *ast.AutoLink:
				label := sanitiseTerminalText(string(n.Label(r.source)))
				spans = appendSpan(spans, label, style+r.theme.MarkdownLink+"\x1b[4m")
			case *ast.RawHTML:
				for i := 0; i < n.Segments.Len(); i++ {
					segment := n.Segments.At(i)
					spans = appendSpan(spans, string(segment.Value(r.source)), style)
				}
			default:
				if n.HasChildren() {
					visit(n, style)
				}
			}
		}
	}
	visit(parent, inherited)
	return spans
}

func markdownTextValue(value string) string {
	// Goldmark leaves backslash escapes and entity references in Text segments;
	// its HTML writer normally resolves them. Do the equivalent for terminal
	// text, without passing through HTML.
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) {
			next := value[i+1]
			if next >= 0x21 && next <= 0x7e && strings.ContainsRune(`!\"#$%&'()*+,-./:;<=>?@[\\]^_`+"`"+`{|}~`, rune(next)) {
				b.WriteByte(next)
				i++
				continue
			}
		}
		b.WriteByte(value[i])
	}
	return html.UnescapeString(b.String())
}

func appendSpan(spans []markdownSpan, text, style string) []markdownSpan {
	text = sanitiseTerminalText(text)
	if text == "" {
		return spans
	}
	if len(spans) > 0 && spans[len(spans)-1].style == style && !spans[len(spans)-1].preserve {
		spans[len(spans)-1].text += text
		return spans
	}
	return append(spans, markdownSpan{text: text, style: style})
}

// wrapMarkdownLine wraps at whitespace where possible, and at rune boundaries
// otherwise.  It retains leading/trailing whitespace for code blocks.
func wrapMarkdownLine(line markdownLine, width int) []markdownLine {
	if width < 1 {
		width = 1
	}
	if line.rule {
		return []markdownLine{{spans: []markdownSpan{{text: strings.Repeat("─", width), style: line.spans[0].style}}, verbatim: true}}
	}
	if line.verbatim {
		return wrapMarkdownVerbatim(line, width)
	}
	var out []markdownLine
	current := markdownLine{}
	used := 0
	pendingSpace := markdownSpan{}
	flush := func(force bool) {
		if len(current.spans) > 0 || force {
			out = append(out, current)
		}
		current, used, pendingSpace = markdownLine{}, 0, markdownSpan{}
	}
	for _, span := range line.spans {
		if span.preserve && strings.TrimSpace(span.text) == "" {
			for range span.text {
				if used >= width {
					flush(false)
				}
				current.spans = appendSpan(current.spans, " ", span.style)
				used++
			}
			continue
		}
		var token strings.Builder
		flushToken := func() {
			word := token.String()
			token.Reset()
			if word == "" {
				return
			}
			for word != "" {
				spaceWidth := 0
				if used > 0 && pendingSpace.text != "" {
					spaceWidth = 1
				}
				// Move a word that fits on an empty line instead of splitting it at
				// the remaining edge of the current line. Only genuinely overlong
				// tokens are broken across rows.
				if used > 0 && runewidth.StringWidth(word) <= width && used+spaceWidth+runewidth.StringWidth(word) > width {
					flush(false)
					spaceWidth = 0
				}
				room := width - used - spaceWidth
				if room <= 0 {
					flush(false)
					room = width
				}
				piece, rest := takeDisplay(word, room)
				if piece == "" { // a wide rune in a one-cell viewport
					_, size := firstRune(word)
					piece, rest = " ", word[size:]
				}
				if used > 0 && pendingSpace.text != "" {
					current.spans = appendSpan(current.spans, " ", pendingSpace.style)
					used++
				}
				current.spans = appendSpan(current.spans, piece, span.style)
				used += runewidth.StringWidth(piece)
				word = rest
				pendingSpace = markdownSpan{}
				if word != "" {
					flush(false)
				}
			}
		}
		for _, ch := range span.text {
			if ch == '\n' {
				flushToken()
				flush(true)
				continue
			}
			if unicode.IsSpace(ch) {
				flushToken()
				pendingSpace = markdownSpan{text: " ", style: span.style}
				continue
			}
			token.WriteRune(ch)
		}
		flushToken()
	}
	if len(current.spans) > 0 || len(out) == 0 {
		flush(true)
	}
	return out
}

func wrapMarkdownVerbatim(line markdownLine, width int) []markdownLine {
	var out []markdownLine
	current := markdownLine{verbatim: true}
	used := 0
	for _, span := range line.spans {
		for _, original := range span.text {
			r := printableRune(original)
			rw := runeWidth(r)
			if rw > width {
				r, rw = ' ', 1
			}
			if used > 0 && used+rw > width {
				out = append(out, current)
				current, used = markdownLine{verbatim: true}, 0
			}
			current.spans = appendSpan(current.spans, string(r), span.style)
			used += rw
		}
	}
	out = append(out, current)
	return out
}

func takeDisplay(s string, width int) (string, string) {
	if width <= 0 {
		return "", s
	}
	used, at := 0, 0
	for i, r := range s {
		rw := runeWidth(r)
		if used+rw > width {
			return s[:at], s[at:]
		}
		used += rw
		at = i + len(string(r))
	}
	return s, ""
}

func firstRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 0
}

func styledMarkdownLine(line markdownLine, base, reset string) string {
	var b strings.Builder
	active := ""
	for _, span := range line.spans {
		if span.text == "" {
			continue
		}
		if span.style != active {
			b.WriteString(reset)
			b.WriteString(base)
			b.WriteString(span.style)
			active = span.style
		}
		b.WriteString(span.text)
	}
	return b.String()
}

func sanitiseTerminalText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\n':
			b.WriteRune(r)
		case '\t':
			b.WriteRune(' ')
		default:
			if unicode.IsControl(r) {
				b.WriteRune(' ')
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
