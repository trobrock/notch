// Package ui contains Notch's dependency-free terminal interface.
package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/trobrock/notch/internal/agent"
)

type Terminal struct {
	reader *bufio.Reader
	out    io.Writer
	errOut io.Writer
	cwd    string
	mu     sync.Mutex
}

func NewTerminal(in io.Reader, out, errOut io.Writer, cwd string) *Terminal {
	return &Terminal{reader: bufio.NewReader(in), out: out, errOut: errOut, cwd: cwd}
}

func (t *Terminal) CWD() string { return t.cwd }

func (t *Terminal) Exec(ctx context.Context, command string, args []string) (string, string, int, error) {
	if command == "" {
		return "", "", -1, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = t.cwd
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	exit := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exit = exitErr.ExitCode()
		} else {
			exit = -1
		}
	}
	return stdout.String(), stderr.String(), exit, err
}

func (t *Terminal) Input(ctx context.Context, prompt, placeholder string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if placeholder != "" {
		fmt.Fprintf(t.out, "%s (%s): ", prompt, placeholder)
	} else {
		fmt.Fprintf(t.out, "%s: ", prompt)
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
	fmt.Fprintln(t.out, prompt)
	for i, option := range options {
		fmt.Fprintf(t.out, "  %d. %s\n", i+1, option)
	}
	fmt.Fprint(t.out, "> ")
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
	fmt.Fprintf(t.errOut, "[%s] %s\n", level, message)
}

func (t *Terminal) FollowUp(string) error {
	return errors.New("extension follow-up is unavailable in line mode")
}
func (t *Terminal) Handoff(string, bool) error {
	return errors.New("extension handoff is unavailable in line mode")
}
func (t *Terminal) SetActiveTools([]string) error {
	return errors.New("runtime tool policy is unavailable in line mode")
}
func (t *Terminal) SwitchModel(context.Context, string, string) (string, int, error) {
	return "", 0, errors.New("runtime model switching is unavailable in line mode")
}
func (t *Terminal) SetStatus(string, string)          {}
func (t *Terminal) SetPanel(string, string, []string) {}

func (t *Terminal) ReadPrompt(label string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprint(t.out, label)
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
		fmt.Fprint(t.out, event.Text)
	case "tool_start":
		fmt.Fprintf(t.errOut, "\n→ %s\n", event.ToolName)
	case "tool_update":
		fmt.Fprintf(t.errOut, "  %s\n", event.Text)
	case "tool_end":
		if event.Result != nil && event.Result.IsError {
			fmt.Fprintf(t.errOut, "  error: %s\n", event.Result.Content)
		}
	case "error":
		fmt.Fprintf(t.errOut, "error: %s\n", event.Text)
	}
}

func DefaultTerminal(cwd string) *Terminal { return NewTerminal(os.Stdin, os.Stdout, os.Stderr, cwd) }
