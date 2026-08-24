package tui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExtensionStatusesAreSortedInFooter(t *testing.T) {
	state := &LayoutState{
		CWD: "~/work", Status: "ready", Provider: "openai", Model: "gpt",
		ThinkingLevel: "off", Statuses: map[string]string{"zeta": "z 2/3", "alpha": "a 1/2"},
	}
	footer := footerText(state, 100)[1]
	if !strings.Contains(footer, "a 1/2 z 2/3") {
		t.Fatalf("footer = %q", footer)
	}
}

func TestAppSetStatusReplacesAndClearsKey(t *testing.T) {
	a := NewApp(AppConfig{CWD: "/tmp"})
	a.SetStatus("tasks", "tasks 1/3")
	if len(a.pending) != 1 || a.pending[0].status == nil {
		t.Fatalf("pending = %#v", a.pending)
	}
	a.applyEvent(nil, a.pending[0])
	if a.state.layout.Statuses["tasks"] != "tasks 1/3" {
		t.Fatalf("statuses = %#v", a.state.layout.Statuses)
	}
	a.SetStatus("tasks", "")
	a.applyEvent(nil, a.pending[1])
	if _, ok := a.state.layout.Statuses["tasks"]; ok {
		t.Fatalf("status was not cleared: %#v", a.state.layout.Statuses)
	}
}

func TestExtensionPanelsRenderAndClear(t *testing.T) {
	state := &LayoutState{
		Width: 50, Height: 12, Editor: NewEditor(), Provider: "openai", Model: "gpt",
		ThinkingLevel: "off", Panels: map[string]ExtensionPanel{
			"tasks": {Key: "tasks", Title: "Tasks 0/2", Lines: []string{"● Implement", "○ Test"}},
		},
	}
	frame := BuildFrame(state)
	joined := strings.Join(frame.Rows, "\n")
	if !strings.Contains(joined, "Tasks 0/2") || !strings.Contains(joined, "● Implement") || !strings.Contains(joined, "○ Test") {
		t.Fatalf("frame = %q", joined)
	}

	a := NewApp(AppConfig{CWD: "/tmp"})
	a.SetPanel("tasks", "Tasks", []string{"one"})
	a.applyEvent(nil, a.pending[0])
	if a.state.layout.Panels["tasks"].Title != "Tasks" {
		t.Fatalf("panels = %#v", a.state.layout.Panels)
	}
	a.SetPanel("tasks", "", nil)
	a.applyEvent(nil, a.pending[1])
	if _, ok := a.state.layout.Panels["tasks"]; ok {
		t.Fatalf("panel was not cleared: %#v", a.state.layout.Panels)
	}
}

func TestSetPanelBoundsPublishedContent(t *testing.T) {
	a := NewApp(AppConfig{CWD: "/tmp"})
	lines := make([]string, maxPanelLines+5)
	for i := range lines {
		lines[i] = strings.Repeat("界", maxPanelLineBytes)
	}
	a.SetPanel("large", "Large", lines)
	panel := a.pending[0].panel
	if len(panel.lines) != maxPanelLines {
		t.Fatalf("lines = %d", len(panel.lines))
	}
	if len(panel.lines[0]) > maxPanelLineBytes || !utf8.ValidString(panel.lines[0]) {
		t.Fatalf("line bytes = %d valid=%v", len(panel.lines[0]), utf8.ValidString(panel.lines[0]))
	}
}
