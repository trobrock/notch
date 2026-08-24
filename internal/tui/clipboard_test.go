package tui

import (
	"bytes"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestWriteOSC52(t *testing.T) {
	var out bytes.Buffer
	if err := writeOSC52(&out, "hello", false, false); err != nil {
		t.Fatal(err)
	}
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("hello")) + "\a"
	if out.String() != want {
		t.Fatalf("OSC 52 = %q, want %q", out.String(), want)
	}
}

func TestWriteOSC52TmuxPassthrough(t *testing.T) {
	var out bytes.Buffer
	if err := writeOSC52(&out, "x", true, false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "\x1b]52;c;") || !strings.Contains(out.String(), "\x1bPtmux;\x1b\x1b]52;c;") || !strings.HasSuffix(out.String(), "\a\x1b\\") {
		t.Fatalf("tmux OSC 52 = %q", out.String())
	}
}

func TestClipboardCommand(t *testing.T) {
	look := func(available ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			for _, candidate := range available {
				if name == candidate {
					return "/bin/" + name, nil
				}
			}
			return "", errors.New("missing")
		}
	}
	env := func(key string) string {
		if key == "WAYLAND_DISPLAY" {
			return "wayland-0"
		}
		return ""
	}
	if command, args := clipboardCommand("linux", env, look("wl-copy", "xclip")); command != "wl-copy" || args != nil {
		t.Fatalf("wayland = %q %#v", command, args)
	}
	if command, args := clipboardCommand("linux", func(string) string { return "" }, look("xclip")); command != "xclip" || !reflect.DeepEqual(args, []string{"-selection", "clipboard"}) {
		t.Fatalf("xclip = %q %#v", command, args)
	}
	if command, args := clipboardCommand("darwin", env, look("pbcopy")); command != "pbcopy" || args != nil {
		t.Fatalf("darwin = %q %#v", command, args)
	}
	if command, args := clipboardCommand("windows", env, look("clip.exe")); command != "clip.exe" || args != nil {
		t.Fatalf("windows = %q %#v", command, args)
	}
}
