package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/trobrock/notch/internal/session"
	"golang.org/x/term"
)

// SelectRecentSession presents a small, in-place picker for a short list of
// recent sessions. It does not enter the alternate screen.
func SelectRecentSession(ctx context.Context, in, out *os.File, infos []session.Info, theme Theme) (session.Info, error) {
	if len(infos) == 0 {
		return session.Info{}, errors.New("no saved sessions for this working directory")
	}
	old, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return session.Info{}, fmt.Errorf("open session picker: %w", err)
	}
	defer term.Restore(int(in.Fd()), old)
	theme = completeTheme(theme, "")
	_, _ = io.WriteString(out, enableBracketedPaste+showCursor)
	defer func() { _, _ = io.WriteString(out, "\r\x1b[J"+disableBracketedPaste+showCursor) }()

	inputCtx, stopInput := context.WithCancel(ctx)
	resize := make(chan os.Signal, 1)
	signalNotify(resize)
	defer signalStop(resize)
	input := make(chan []KeyEvent, 8)
	readErrors := make(chan error, 1)
	inputDone := make(chan struct{})
	reader := &App{cfg: AppConfig{In: in}}
	go func() {
		reader.readInput(inputCtx, input, readErrors)
		close(inputDone)
	}()
	defer func() {
		stopInput()
		nonblocking, interruptErr := interruptTerminalRead(in)
		if interruptErr == nil {
			<-inputDone
			_ = restoreTerminalRead(in, nonblocking)
		}
	}()

	query, selected := "", 0
	for {
		matches := recentSessionMatches(infos, query)
		if selected >= len(matches) {
			selected = max(0, len(matches)-1)
		}
		width, _, sizeErr := term.GetSize(int(out.Fd()))
		if sizeErr != nil || width <= 0 {
			width = 80
		}
		rows, cursorCol := recentSessionPickerRows(infos, matches, query, selected, width, theme)
		_, _ = io.WriteString(out, hideCursor+"\r\x1b[J"+strings.Join(rows, "\r\n")+"\r\n")
		_, _ = fmt.Fprintf(out, "\x1b[%dA\r\x1b[%dC%s", len(rows), cursorCol, showCursor)

		var keys []KeyEvent
		select {
		case <-ctx.Done():
			return session.Info{}, ctx.Err()
		case <-resize:
			continue
		case readErr := <-readErrors:
			return session.Info{}, readErr
		case keys = <-input:
		}
		for _, key := range keys {
			matches = recentSessionMatches(infos, query)
			if selected >= len(matches) {
				selected = max(0, len(matches)-1)
			}
			switch key.Key {
			case KeyEscape, KeyCtrlC:
				return session.Info{}, context.Canceled
			case KeyUp:
				if selected > 0 {
					selected--
				}
			case KeyDown:
				if selected+1 < len(matches) {
					selected++
				}
			case KeyBackspace:
				if query != "" {
					_, size := utf8.DecodeLastRuneInString(query)
					query = query[:len(query)-size]
					selected = 0
				}
			case KeyCtrlU:
				query, selected = "", 0
			case KeyEnter:
				if len(matches) != 0 {
					return infos[matches[selected]], nil
				}
			}
			if key.Text != "" {
				text := sanitiseTerminalText(key.Text)
				if key.Paste {
					text = strings.Join(strings.Fields(text), " ")
				}
				query += text
				selected = 0
			}
		}
	}
}

func recentSessionMatches(infos []session.Info, query string) []int {
	query = strings.ToLower(strings.TrimSpace(query))
	var matches []int
	for i, info := range infos {
		text := strings.ToLower(strings.Join([]string{info.Preview, info.Header.ID, info.Header.Model}, " "))
		if query == "" || strings.Contains(text, query) {
			matches = append(matches, i)
		}
	}
	return matches
}

func recentSessionPickerRows(infos []session.Info, matches []int, query string, selected, width int, theme Theme) ([]string, int) {
	query = sanitiseTerminalText(query)
	prompt := theme.Accent + "❯ " + theme.Reset + query
	rows := []string{padANSI(prompt, width)}
	for position, index := range matches {
		info := infos[index]
		marker, titleStyle := "  ", theme.Text
		if position == selected {
			marker, titleStyle = "› ", theme.Accent+"\x1b[1m"
		}
		age := compactSessionAge(time.Since(info.ModifiedAt))
		count := fmt.Sprintf("%d msgs", info.MessageCount)
		leftWidth := max(1, width-visibleWidth(count)-2)
		titleWidth := max(1, leftWidth-7)
		title := truncateANSI(sanitiseTerminalText(info.Preview), titleWidth)
		left := theme.Muted + marker + fmt.Sprintf("%-4s ", age) + theme.Reset + titleStyle + title + theme.Reset
		right := theme.Muted + count + theme.Reset
		rows = append(rows, overlayRight(left, right, width))
	}
	if len(matches) == 0 {
		rows = append(rows, padANSI(theme.Muted+"  No matching sessions"+theme.Reset, width))
	}
	hint := theme.Muted + "↑↓ move · enter resume · esc cancel" + theme.Reset
	matchCount := theme.Muted + fmt.Sprintf("%d/%d", len(matches), len(infos)) + theme.Reset
	rows = append(rows, overlayRight(hint, matchCount, width))
	return rows, min(width-1, 2+visibleWidth(query))
}

func compactSessionAge(age time.Duration) string {
	if age < time.Minute {
		return "now"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm", int(age.Minutes()))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh", int(age.Hours()))
	}
	return fmt.Sprintf("%dd", int(age.Hours()/24))
}
