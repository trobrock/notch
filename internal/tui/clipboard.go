package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
)

func copyToClipboard(ctx context.Context, out *os.File, text string) error {
	oscErr := writeOSC52(out, text, os.Getenv("TMUX") != "", os.Getenv("STY") != "")
	if os.Getenv("TMUX") != "" {
		cmd := exec.CommandContext(ctx, "tmux", "load-buffer", "-w", "-")
		cmd.Stdin = bytes.NewBufferString(text)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	if command, args := clipboardCommand(runtime.GOOS, os.Getenv, exec.LookPath); command != "" {
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Stdin = bytes.NewBufferString(text)
		if output, err := cmd.CombinedOutput(); err == nil {
			return nil
		} else if oscErr != nil {
			return errors.Join(oscErr, fmt.Errorf("%s: %w (%s)", command, err, bytes.TrimSpace(output)))
		}
	}
	if oscErr == nil {
		return nil
	}
	return errors.Join(oscErr, errors.New("no clipboard helper available"))
}

func writeOSC52(out io.Writer, text string, tmux, screen bool) error {
	if out == nil {
		return errors.New("terminal output is unavailable")
	}
	sequence := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
	if tmux {
		sequence = sequence + "\x1bPtmux;\x1b" + sequence + "\x1b\\"
	} else if screen {
		sequence = "\x1bP\x1b" + sequence + "\x1b\\"
	}
	_, err := io.WriteString(out, sequence)
	return err
}

func clipboardCommand(goos string, getenv func(string) string, lookPath func(string) (string, error)) (string, []string) {
	has := func(name string) bool { _, err := lookPath(name); return err == nil }
	switch goos {
	case "darwin":
		if has("pbcopy") {
			return "pbcopy", nil
		}
	case "windows":
		if has("powershell.exe") {
			return "powershell.exe", []string{"-NoProfile", "-NonInteractive", "-Command", "Set-Clipboard -Value ([Console]::In.ReadToEnd())"}
		}
		if has("clip.exe") {
			return "clip.exe", nil
		}
	default:
		if getenv("WAYLAND_DISPLAY") != "" && has("wl-copy") {
			return "wl-copy", nil
		}
		if has("xclip") {
			return "xclip", []string{"-selection", "clipboard"}
		}
		if has("xsel") {
			return "xsel", []string{"--clipboard", "--input"}
		}
		if has("clip.exe") {
			return "clip.exe", nil
		}
	}
	return "", nil
}
