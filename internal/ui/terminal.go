// Package ui contains Notch's dependency-free terminal interface.
package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/term"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/extension"
	sharedprocess "github.com/trobrock/notch/internal/process"
	"github.com/trobrock/notch/internal/session"
)

type Terminal struct {
	reader    *bufio.Reader
	out       io.Writer
	errOut    io.Writer
	cwd       string
	mu        sync.Mutex
	sessionMu sync.RWMutex
	session   *session.Session
	registry  *extension.Registry

	outTTY      bool
	errTTY      bool
	outSanitize terminalSanitizer
}

func NewTerminal(in io.Reader, out, errOut io.Writer, cwd string) *Terminal {
	return &Terminal{
		reader: bufio.NewReader(in), out: out, errOut: errOut, cwd: cwd,
		outTTY: isTerminalWriter(out), errTTY: isTerminalWriter(errOut),
	}
}

func isTerminalWriter(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func (t *Terminal) writeOut(text string) {
	if t.outTTY {
		var sanitizer terminalSanitizer
		text = sanitizer.clean(text)
	}
	_, _ = io.WriteString(t.out, text)
}

func (t *Terminal) writeStreamOut(text string) {
	if t.outTTY {
		text = t.outSanitize.clean(text)
	}
	_, _ = io.WriteString(t.out, text)
}

func (t *Terminal) writeErr(text string) {
	if t.errTTY {
		var sanitizer terminalSanitizer
		text = sanitizer.clean(text)
	}
	_, _ = io.WriteString(t.errOut, text)
}

// SetSession supplies the current session used by extension persistence APIs.
func (t *Terminal) SetSession(current *session.Session) {
	t.sessionMu.Lock()
	t.session = current
	t.sessionMu.Unlock()
}

func (t *Terminal) CWD() string { return t.cwd }

func (t *Terminal) Exec(ctx context.Context, command string, args []string) (string, string, int, error) {
	return sharedprocess.Run(ctx, t.cwd, command, args)
}

func (t *Terminal) Input(ctx context.Context, prompt, placeholder string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if placeholder != "" {
		t.writeOut(fmt.Sprintf("%s (%s): ", prompt, placeholder))
	} else {
		t.writeOut(fmt.Sprintf("%s: ", prompt))
	}
	line, err := t.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	value := strings.TrimSpace(line)
	if value == "" {
		value = placeholder
	}
	return value, nil
}

func (t *Terminal) Select(ctx context.Context, prompt string, options []string) (string, error) {
	if len(options) == 0 {
		return "", errors.New("select requires options")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	t.writeOut(prompt + "\n")
	for i, option := range options {
		t.writeOut(fmt.Sprintf("  %d. %s\n", i+1, option))
	}
	t.writeOut("> ")
	line, err := t.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	index, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || index < 1 || index > len(options) {
		return "", errors.New("invalid selection")
	}
	return options[index-1], nil
}

func (t *Terminal) Notify(message, level string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if level == "" {
		level = "info"
	}
	t.writeErr(fmt.Sprintf("[%s] %s\n", level, message))
}

func (t *Terminal) FollowUp(string) error {
	return errors.New("extension follow-up is unavailable in line mode")
}
func (t *Terminal) Handoff(string, bool) error {
	return errors.New("extension handoff is unavailable in line mode")
}
func (t *Terminal) SetRegistry(registry *extension.Registry) { t.registry = registry }

func (t *Terminal) SetActiveTools(names []string) error {
	if t.registry == nil {
		return errors.New("runtime tool policy is unavailable in line mode")
	}
	if missing := t.registry.SetActiveTools(names); len(missing) != 0 {
		return fmt.Errorf("unknown tools: %s", strings.Join(missing, ", "))
	}
	return nil
}
func (t *Terminal) SwitchModel(context.Context, string, string) (string, int, error) {
	return "", 0, errors.New("runtime model switching is unavailable in line mode")
}
func (t *Terminal) ListModels(context.Context, string, bool) ([]extension.ModelInfo, error) {
	return nil, errors.New("runtime model listing is unavailable in line mode")
}
func (t *Terminal) AppendSessionEntry(kind string, data any) error {
	t.sessionMu.RLock()
	current := t.session
	t.sessionMu.RUnlock()
	if current == nil {
		return errors.New("session persistence is unavailable")
	}
	return current.AppendCustomEntry(kind, data)
}
func (t *Terminal) SessionEntries(kind string) ([]json.RawMessage, error) {
	t.sessionMu.RLock()
	current := t.session
	t.sessionMu.RUnlock()
	if current == nil {
		return nil, errors.New("session persistence is unavailable")
	}
	return current.CustomEntries(kind)
}
func (t *Terminal) EditorText(context.Context) (string, error) {
	return "", errors.New("prompt editor is unavailable in line mode")
}
func (t *Terminal) SetEditorText(context.Context, string) error {
	return errors.New("prompt editor is unavailable in line mode")
}
func (t *Terminal) SetStatus(string, string)          {}
func (t *Terminal) SetPanel(string, string, []string) {}

func (t *Terminal) ReadPrompt(label string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.writeOut(label)
	line, err := t.reader.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (t *Terminal) Render(event agent.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch event.Type {
	case "text_delta":
		t.writeStreamOut(event.Text)
	case "tool_start":
		t.writeErr(fmt.Sprintf("\n→ %s\n", event.ToolName))
	case "tool_update":
		t.writeErr(fmt.Sprintf("  %s\n", event.Text))
	case "tool_end":
		if event.Result != nil && event.Result.IsError {
			t.writeErr(fmt.Sprintf("  error: %s\n", event.Result.Content))
		}
	case "error":
		t.writeErr(fmt.Sprintf("error: %s\n", event.Text))
	}
}

func DefaultTerminal(cwd string) *Terminal { return NewTerminal(os.Stdin, os.Stdout, os.Stderr, cwd) }
