package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/modelregistry"
	"github.com/trobrock/notch/internal/resources"
	"github.com/trobrock/notch/internal/session"
)

type appTestProvider struct{}

func (appTestProvider) Stream(context.Context, model.Request, func(model.StreamEvent)) (model.Response, error) {
	return model.Response{Content: []model.Block{{Type: "text", Text: "ok"}}}, nil
}

func newAppTestAgent(t *testing.T, store *session.Session) *agent.Agent {
	t.Helper()
	runner, err := agent.New(agent.Config{
		Provider: appTestProvider{}, Registry: extension.NewRegistry(), Session: store, Model: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestAppImplementsExtensionHost(t *testing.T) {
	var _ extension.Host = NewApp(AppConfig{CWD: "/tmp"})
}

func TestAppKeyEditingAndBusySubmission(t *testing.T) {
	a := NewApp(AppConfig{})
	a.state.layout.Height = 20

	for _, event := range []KeyEvent{{Text: "one"}, {Key: KeyNewline}, {Text: "two"}, {Key: KeyCtrlU}} {
		a.handleKey(context.Background(), event)
	}
	if got := a.state.editor.Text(); got != "one\n" {
		t.Fatalf("edited text = %q, want %q", got, "one\n")
	}

	a.state.activeModel = true
	changed, exit := a.handleKey(context.Background(), KeyEvent{Key: KeyEnter})
	if !changed || exit {
		t.Fatalf("Enter while streaming changed=%v exit=%v", changed, exit)
	}
	if got := a.state.editor.Text(); got != "one\n" {
		t.Fatalf("failed queue discarded draft: %q", got)
	}

	changed, exit = a.handleKey(context.Background(), KeyEvent{Key: KeyCtrlC})
	if !changed || exit || a.state.layout.Status != "canceling" {
		t.Fatalf("Ctrl-C while active changed=%v exit=%v status=%q", changed, exit, a.state.layout.Status)
	}
}

func TestTranscriptScrollingKeysClampToContent(t *testing.T) {
	a := NewApp(AppConfig{})
	a.state.layout.Width, a.state.layout.Height = 40, 10
	for i := 0; i < 30; i++ {
		a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: TranscriptAssistant, Text: fmt.Sprintf("line %d", i)})
	}
	if changed, _ := a.handleKey(context.Background(), KeyEvent{Key: KeyScrollUp}); !changed || a.state.layout.ScrollOffset != 3 {
		t.Fatalf("wheel up offset = %d, changed=%v", a.state.layout.ScrollOffset, changed)
	}
	for i := 0; i < 100; i++ {
		a.handleKey(context.Background(), KeyEvent{Key: KeyPageUp})
	}
	limit := transcriptScrollLimit(&a.state.layout)
	if a.state.layout.ScrollOffset != limit {
		t.Fatalf("offset = %d, limit = %d", a.state.layout.ScrollOffset, limit)
	}
	if changed, _ := a.handleKey(context.Background(), KeyEvent{Key: KeyPageUp}); changed {
		t.Fatal("scroll changed past top")
	}
	if changed, _ := a.handleKey(context.Background(), KeyEvent{Key: KeyScrollDown}); !changed || a.state.layout.ScrollOffset != limit-3 {
		t.Fatalf("wheel down offset = %d", a.state.layout.ScrollOffset)
	}
}

func TestTranscriptPageUsesActualViewport(t *testing.T) {
	a := NewApp(AppConfig{})
	a.state.layout.Width, a.state.layout.Height = 40, 20
	a.state.layout.Panels["p"] = ExtensionPanel{Key: "p", Lines: []string{"one", "two", "three", "four"}}
	for i := 0; i < 30; i++ {
		a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: TranscriptAssistant, Text: fmt.Sprintf("line %d", i)})
	}
	want := max(1, transcriptViewportHeight(&a.state.layout)-1)
	if changed, _ := a.handleKey(context.Background(), KeyEvent{Key: KeyPageUp}); !changed || a.state.layout.ScrollOffset != want {
		t.Fatalf("page offset = %d, want %d", a.state.layout.ScrollOffset, want)
	}
}

func TestScrolledTranscriptStaysAnchoredDuringStreaming(t *testing.T) {
	a := NewApp(AppConfig{})
	a.state.layout.Width, a.state.layout.Height = 20, 10
	for i := 0; i < 30; i++ {
		a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: TranscriptAssistant, Text: fmt.Sprintf("line %d", i)})
	}
	a.state.assistant = len(a.state.layout.Transcript) - 1
	a.state.layout.ScrollOffset = 6
	oldLines := a.transcriptRenderedLines()
	a.applyEvent(context.Background(), appEvent{agent: &agent.Event{Type: "text_delta", Text: "\nextra wrapped content"}})
	if a.state.layout.ScrollOffset <= 6 || a.state.layout.ScrollOffset != 6+a.transcriptRenderedLines()-oldLines {
		t.Fatalf("scroll offset did not preserve position: %d", a.state.layout.ScrollOffset)
	}

	a.state.layout.ScrollOffset = 0
	a.applyEvent(context.Background(), appEvent{agent: &agent.Event{Type: "text_delta", Text: "\nfollow tail"}})
	if a.state.layout.ScrollOffset != 0 {
		t.Fatalf("tail following offset = %d", a.state.layout.ScrollOffset)
	}
}

func TestScrolledTranscriptAnchorHandlesLayoutChanges(t *testing.T) {
	a := NewApp(AppConfig{})
	a.state.layout.Width, a.state.layout.Height = 30, 15
	for i := 0; i < 30; i++ {
		a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: TranscriptAssistant, Text: fmt.Sprintf("line %d", i)})
	}
	a.state.layout.ScrollOffset = 8
	oldLines := a.transcriptRenderedLines()
	oldViewport := transcriptViewportHeight(&a.state.layout)
	a.state.layout.Editor.SetText("one\ntwo\nthree")
	a.preserveTranscriptAnchor(oldLines, oldViewport)
	if a.state.layout.ScrollOffset != 10 {
		t.Fatalf("composer growth offset = %d, want 10", a.state.layout.ScrollOffset)
	}

	oldLines = a.transcriptRenderedLines()
	oldViewport = transcriptViewportHeight(&a.state.layout)
	a.state.layout.Transcript = a.state.layout.Transcript[:20]
	a.preserveTranscriptAnchor(oldLines, oldViewport)
	if a.state.layout.ScrollOffset >= 10 || a.state.layout.ScrollOffset > transcriptScrollLimit(&a.state.layout) {
		t.Fatalf("shrink left invalid offset %d", a.state.layout.ScrollOffset)
	}
}

func TestInterruptTerminalReadUnblocksReader(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	done := make(chan error, 1)
	go func() { var b [1]byte; _, err := read.Read(b[:]); done <- err }()
	nonblocking, err := interruptTerminalRead(read)
	if err != nil {
		t.Skipf("input interruption unsupported: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("interrupt did not unblock read")
	}
	if err := restoreTerminalRead(read, nonblocking); err != nil {
		t.Fatal(err)
	}
}

func TestReadInputKeepsSplitMouseSequenceTogether(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	a := NewApp(AppConfig{In: read})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := make(chan []KeyEvent, 1)
	readErrors := make(chan error, 1)
	done := make(chan struct{})
	go func() { a.readInput(ctx, input, readErrors); close(done) }()
	if _, err := write.Write([]byte("\x1b")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := write.Write([]byte("[<64;10;5M")); err != nil {
		t.Fatal(err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case keys := <-input:
		if len(keys) != 1 || keys[0].Key != KeyScrollUp {
			t.Fatalf("keys = %#v", keys)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for mouse event")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("readInput did not exit")
	}
}

func TestReadInputFlushesLiteralEscape(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	a := NewApp(AppConfig{In: read})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := make(chan []KeyEvent, 1)
	readErrors := make(chan error, 1)
	go a.readInput(ctx, input, readErrors)
	if _, err := write.Write([]byte("\x1b")); err != nil {
		t.Fatal(err)
	}
	select {
	case keys := <-input:
		if len(keys) != 1 || keys[0].Key != KeyEscape {
			t.Fatalf("keys = %#v", keys)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Escape")
	}
	select {
	case err := <-readErrors:
		t.Fatalf("unexpected input error before close: %v", err)
	default:
	}
}

func TestComposerQueuesSteeringAndFollowUp(t *testing.T) {
	provider := &appQueueProvider{started: make(chan struct{}), release: make(chan struct{})}
	runner, err := agent.New(agent.Config{Provider: provider, Registry: extension.NewRegistry(), Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runner.Prompt(context.Background(), "initial", nil) }()
	<-provider.started
	a := NewApp(AppConfig{})
	a.runner = runner
	a.state.activeModel = true
	a.state.editor.SetText("change direction")
	if changed, _ := a.handleKey(context.Background(), KeyEvent{Key: KeyEnter}); !changed {
		t.Fatal("steering did not queue")
	}
	a.state.editor.SetText("then summarize")
	if changed, _ := a.handleKey(context.Background(), KeyEvent{Key: KeyAltEnter}); !changed {
		t.Fatal("follow-up did not queue")
	}
	if len(a.state.layout.PendingMessages) != 2 || a.state.layout.PendingMessages[0].Mode != "steer" || a.state.layout.PendingMessages[1].Mode != "follow_up" || a.state.editor.Text() != "" {
		t.Fatalf("pending messages = %#v", a.state.layout.PendingMessages)
	}
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type appQueueProvider struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *appQueueProvider) Stream(ctx context.Context, _ model.Request, _ func(model.StreamEvent)) (model.Response, error) {
	p.once.Do(func() {
		close(p.started)
		select {
		case <-p.release:
		case <-ctx.Done():
		}
	})
	if err := ctx.Err(); err != nil {
		return model.Response{}, err
	}
	return model.Response{Content: []model.Block{{Type: "text", Text: "done"}}, StopReason: "end_turn"}, nil
}

func TestSlashCommandCompletionAndHelp(t *testing.T) {
	a := NewApp(AppConfig{})
	a.catalog = &resources.Catalog{
		Skills:    map[string]resources.Skill{"review": {Description: "review changes"}},
		Templates: map[string]resources.Template{"fix": {Description: "fix issue", ArgumentHint: "<issue>"}},
	}
	registry := extension.NewRegistry()
	if err := registry.RegisterCommand(extension.Command{Name: "deploy", Description: "deploy it", Source: "test", Execute: func(context.Context, string) (string, error) { return "", nil }}); err != nil {
		t.Fatal(err)
	}
	a.registry = registry

	a.handleKey(context.Background(), KeyEvent{Text: "/th"})
	if len(a.state.layout.CommandSuggestions) != 2 || a.state.layout.CommandSuggestions[0].Name != "theme" {
		t.Fatalf("filtered suggestions = %#v", a.state.layout.CommandSuggestions)
	}
	a.handleKey(context.Background(), KeyEvent{Key: KeyDown})
	a.handleKey(context.Background(), KeyEvent{Key: KeyTab})
	if got := a.state.editor.Text(); got != "/thinking " {
		t.Fatalf("accepted completion = %q", got)
	}
	if len(a.state.layout.CommandSuggestions) != 0 {
		t.Fatal("completion remained open after acceptance")
	}

	a.state.editor.SetText("/help")
	a.refreshCommandCompletion()
	a.handleKey(context.Background(), KeyEvent{Key: KeyEnter})
	if !a.state.commandHelp || len(a.state.layout.CommandSuggestions) < 10 || a.state.editor.Text() != "" {
		t.Fatalf("help palette = help:%v suggestions:%#v text:%q", a.state.commandHelp, a.state.layout.CommandSuggestions, a.state.editor.Text())
	}
	found := map[string]bool{}
	for _, item := range a.state.layout.CommandSuggestions {
		found[item.Name] = true
	}
	for _, name := range []string{"deploy", "fix", "skill:review", "resume"} {
		if !found[name] {
			t.Errorf("help palette missing %q", name)
		}
	}
}

func TestAppHistoryAtEditorBoundaries(t *testing.T) {
	a := NewApp(AppConfig{})
	a.state.editor.AddHistory("old prompt")
	a.state.editor.SetText("draft")

	changed, _ := a.handleKey(context.Background(), KeyEvent{Key: KeyUp})
	if !changed || a.state.editor.Text() != "old prompt" {
		t.Fatalf("up did not select history: changed=%v text=%q", changed, a.state.editor.Text())
	}
	changed, _ = a.handleKey(context.Background(), KeyEvent{Key: KeyDown})
	if !changed || a.state.editor.Text() != "draft" {
		t.Fatalf("down did not restore draft: changed=%v text=%q", changed, a.state.editor.Text())
	}
}

func TestAppAgentEventsBuildTranscriptAndUsage(t *testing.T) {
	a := NewApp(AppConfig{})
	a.handleAgentEvent(agent.Event{Type: "turn_start"})
	a.handleAgentEvent(agent.Event{Type: "text_delta", Text: "hello "})
	a.handleAgentEvent(agent.Event{Type: "text_delta", Text: "world"})
	a.handleAgentEvent(agent.Event{Type: "turn_end", Usage: &agent.Usage{InputTokens: 12, OutputTokens: 3}})
	a.handleAgentEvent(agent.Event{Type: "tool_start", ToolName: "shell", ToolCallID: "1"})
	a.handleAgentEvent(agent.Event{Type: "tool_update", ToolName: "shell", ToolCallID: "1", Text: "running ls"})
	a.handleAgentEvent(agent.Event{Type: "tool_end", ToolName: "shell", ToolCallID: "1", Result: &extension.ToolResult{Content: "failed", IsError: true}})

	if len(a.state.layout.Transcript) != 2 {
		t.Fatalf("transcript entries = %d, want 2", len(a.state.layout.Transcript))
	}
	if got := a.state.layout.Transcript[0].Text; got != "hello world" {
		t.Fatalf("assistant text = %q", got)
	}
	tool := a.state.layout.Transcript[1]
	if tool.Kind != KindTool || tool.Text != "failed" || !tool.Error || tool.Pending {
		t.Fatalf("tool entry = %#v", tool)
	}
	if got := a.state.layout.Usage; got != "12 in / 3 out" {
		t.Fatalf("usage = %q", got)
	}
}

func TestAppThinkingSummaryAndFallbackIndicator(t *testing.T) {
	a := NewApp(AppConfig{ThinkingLevel: "medium"})
	a.handleAgentEvent(agent.Event{Type: "turn_start"})
	if !a.pendingThinkingIndicator() {
		t.Fatalf("thinking indicator is not active: %#v", a.state.layout.Transcript)
	}
	if len(a.state.layout.Transcript) != 1 || a.state.layout.Transcript[0].Kind != KindThinking || !a.state.layout.Transcript[0].Pending {
		t.Fatalf("missing thinking indicator: %#v", a.state.layout.Transcript)
	}
	a.handleAgentEvent(agent.Event{Type: "thinking_delta", Text: "Checking "})
	if a.pendingThinkingIndicator() {
		t.Fatal("spinner remained active after thinking text arrived")
	}
	a.handleAgentEvent(agent.Event{Type: "thinking_delta", Text: "the files."})
	a.handleAgentEvent(agent.Event{Type: "text_delta", Text: "Done"})
	a.handleAgentEvent(agent.Event{Type: "turn_end"})
	if len(a.state.layout.Transcript) != 2 || a.state.layout.Transcript[0].Text != "Checking the files." || a.state.layout.Transcript[0].Pending || a.state.layout.Transcript[1].Kind != KindAssistant {
		t.Fatalf("thinking summary transcript: %#v", a.state.layout.Transcript)
	}

	fallback := NewApp(AppConfig{ThinkingLevel: "medium"})
	fallback.handleAgentEvent(agent.Event{Type: "turn_start"})
	fallback.handleAgentEvent(agent.Event{Type: "text_delta", Text: "No summary"})
	if len(fallback.state.layout.Transcript) != 1 || fallback.state.layout.Transcript[0].Kind != KindAssistant {
		t.Fatalf("fallback indicator was not removed: %#v", fallback.state.layout.Transcript)
	}

	off := NewApp(AppConfig{ThinkingLevel: "off"})
	off.handleAgentEvent(agent.Event{Type: "turn_start"})
	if len(off.state.layout.Transcript) != 0 {
		t.Fatalf("off-level indicator = %#v", off.state.layout.Transcript)
	}
}

func TestFinishResumeRestoresTranscriptAndHistory(t *testing.T) {
	dir := t.TempDir()
	old, err := session.New(dir, "/old", "p", "m")
	if err != nil {
		t.Fatal(err)
	}
	next, err := session.New(dir, "/next", "p", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := next.AppendMessage(model.TextMessage("user", "restored prompt")); err != nil {
		t.Fatal(err)
	}
	if err := next.AppendMessage(model.TextMessage("assistant", "restored answer")); err != nil {
		t.Fatal(err)
	}
	path := next.Path()
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := session.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	a := NewApp(AppConfig{SessionDir: dir})
	runner := newAppTestAgent(t, old)
	a.runner, a.currentSession = runner, old
	if !a.finishResume(resumeResult{session: loaded}) {
		t.Fatal("resume reported no change")
	}
	if a.currentSession != loaded || len(a.state.layout.Transcript) != 2 {
		t.Fatalf("resumed state = current:%p transcript:%#v", a.currentSession, a.state.layout.Transcript)
	}
	a.state.editor.SetText("draft")
	if !a.state.editor.HistoryPrevious() || a.state.editor.Text() != "restored prompt" {
		t.Fatalf("restored prompt history = %q", a.state.editor.Text())
	}
	if err := old.AppendMessage(model.TextMessage("user", "closed")); err == nil {
		t.Fatal("old session remained open")
	}
	_ = loaded.Close()
}

func TestAppModalRoutesKeysAndRestoresDraft(t *testing.T) {
	a := NewApp(AppConfig{})
	a.state.editor.SetText("draft while streaming")
	request := &hostRequest{
		kind: "select", ctx: context.Background(), prompt: "Choose", options: []string{"a", "b"},
		reply: make(chan hostResponse, 1),
	}
	if !a.enqueueHostRequest(request) {
		t.Fatal("host request did not change state")
	}
	if entry := a.state.layout.Transcript[len(a.state.layout.Transcript)-1]; entry.Kind != KindPrompt || !strings.Contains(entry.Text, "↑/↓ navigate") || !strings.Contains(entry.Text, "❯ ● a") {
		t.Fatalf("prompt entry = %#v", entry)
	}
	if a.state.layout.Editor == a.state.editor {
		t.Fatal("modal did not install its own editor")
	}
	a.handleModalKey(KeyEvent{Key: KeyDown})
	a.handleModalKey(KeyEvent{Key: KeyEnter})

	response := <-request.reply
	if response.err != nil || response.value != "b" {
		t.Fatalf("response = %#v", response)
	}
	if a.state.layout.Editor != a.state.editor || a.state.editor.Text() != "draft while streaming" {
		t.Fatalf("draft was not restored: %q", a.state.editor.Text())
	}
	joined := transcriptText(a.state.layout.Transcript)
	if !strings.Contains(joined, "Choose") || !strings.Contains(joined, "selection") {
		t.Fatalf("modal was not represented in transcript: %q", joined)
	}
}

func TestSearchableSelectFiltersLargeModelLists(t *testing.T) {
	a := NewApp(AppConfig{})
	options := make([]string, 30)
	for i := range options {
		options[i] = fmt.Sprintf("model-%02d", i)
	}
	options[23] = "target-model"
	request := &hostRequest{kind: "select", ctx: context.Background(), prompt: "Choose model", options: options, reply: make(chan hostResponse, 1)}
	a.enqueueHostRequest(request)
	a.handleModalKey(KeyEvent{Text: "target"})
	if request.selected != 23 || !strings.Contains(request.render(), "Filter: target") || strings.Contains(request.render(), "model-00") {
		t.Fatalf("filtered selector = selected:%d render:%q", request.selected, request.render())
	}
	a.handleModalKey(KeyEvent{Key: KeyEnter})
	if response := <-request.reply; response.err != nil || response.value != "target-model" {
		t.Fatalf("response = %#v", response)
	}
}

func TestFinishModelSelectionUpdatesRuntimeDisplay(t *testing.T) {
	a := NewApp(AppConfig{Provider: "old", Model: "old-model"})
	entry := modelregistry.Entry{Provider: "openai", ID: "gpt-test", Name: "GPT Test", ContextWindow: 123000, Reasoning: true}
	if label := modelSelectionLabel(entry); !strings.Contains(label, "gpt-test") || !strings.Contains(label, "123k ctx") || !strings.Contains(label, "reasoning") {
		t.Fatalf("model label = %q", label)
	}
	a.state.activeCommand = true
	if !a.finishModelSelection(modelSelectionResult{provider: "openai", model: "gpt-test", contextWindow: 123000}) {
		t.Fatal("selection reported no change")
	}
	if a.state.layout.Provider != "openai" || a.state.layout.Model != "gpt-test" || a.state.layout.ContextWindow != 123000 || a.state.activeCommand {
		t.Fatalf("model state = %#v", a.state.layout)
	}
}

func TestAppBuiltinsAndPreRunNotifyDoNotStartTimers(t *testing.T) {
	a := NewApp(AppConfig{})
	timerCalls := 0
	// The timer factory is only consulted by Run after a streamed delta. Constructing
	// and mutating idle state must not install a ticker or periodic timer.
	a.newTimer = func(_ time.Duration) *time.Timer { timerCalls++; return nil }
	_ = timerCalls

	// Notify is safe before the event loop and is retained rather than blocking
	// on stdin or competing with its reader.
	a.Notify("loaded early", "warning")
	if len(a.pending) != 1 || a.pending[0].notice == nil {
		t.Fatalf("pre-run notice was not retained: %#v", a.pending)
	}

	a.submit(context.Background(), "/help")
	a.submit(context.Background(), "/clear")
	if len(a.state.layout.Transcript) != 0 {
		t.Fatalf("clear left %d transcript entries", len(a.state.layout.Transcript))
	}
	if timerCalls != 0 {
		t.Fatalf("idle helpers started %d timers", timerCalls)
	}
}

func TestAppThinkingCycleForwardsToAgent(t *testing.T) {
	a := NewApp(AppConfig{})
	runner := newAppTestAgent(t, nil)
	a.Configure(runner, extension.NewRegistry(), nil)

	for _, want := range []string{"minimal", "low", "medium", "high", "xhigh", "off"} {
		changed, exit := a.handleKey(context.Background(), KeyEvent{Key: KeyShiftTab})
		if !changed || exit {
			t.Fatalf("Shift-Tab changed=%v exit=%v", changed, exit)
		}
		if got := runner.ThinkingLevel(); got != want {
			t.Fatalf("runner thinking = %q, want %q", got, want)
		}
		if got := a.state.layout.ThinkingLevel; got != want {
			t.Fatalf("layout thinking = %q, want %q", got, want)
		}
	}
}

func TestAppManualCompactionEventsShareNotice(t *testing.T) {
	a := NewApp(AppConfig{})
	before := agent.ContextUsage{Tokens: 900, ContextWindow: 1000, AutoCompact: true}
	a.handleAgentEvent(agent.Event{Type: "compaction_start", ContextUsage: &before})
	if len(a.state.layout.Transcript) != 1 || a.state.layout.Status != "compacting" {
		t.Fatalf("compaction start state = %#v", a.state)
	}
	if entry := a.state.layout.Transcript[0]; !entry.Pending || !strings.Contains(entry.Text, "manually") {
		t.Fatalf("compaction start entry = %#v", entry)
	}

	after := agent.ContextUsage{Tokens: 200, ContextWindow: 1000, AutoCompact: true}
	a.handleAgentEvent(agent.Event{Type: "compaction_end", ContextUsage: &after})
	if len(a.state.layout.Transcript) != 1 {
		t.Fatalf("compaction created %d notices, want one", len(a.state.layout.Transcript))
	}
	if entry := a.state.layout.Transcript[0]; entry.Pending || !strings.Contains(entry.Text, "compacted manually") {
		t.Fatalf("compaction end entry = %#v", entry)
	}
	if a.state.layout.ContextTokens != 200 {
		t.Fatalf("context tokens = %d", a.state.layout.ContextTokens)
	}
}

func TestAppNewCreatesFreshSession(t *testing.T) {
	dir := t.TempDir()
	old, err := session.New(dir, "/work", "test", "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := old.AppendMessage(model.TextMessage("user", "old context")); err != nil {
		t.Fatal(err)
	}
	runner := newAppTestAgent(t, old)
	a := NewApp(AppConfig{})
	a.Configure(runner, extension.NewRegistry(), nil)
	var fresh *session.Session
	a.SetSessionFactory(old, func() (*session.Session, error) {
		var createErr error
		fresh, createErr = session.New(dir, "/work", "test", "model")
		return fresh, createErr
	})
	a.state.editor.AddHistory("old prompt")

	a.submit(context.Background(), "/new")
	if fresh == nil || len(runner.Messages()) != 0 {
		t.Fatalf("fresh session not installed: fresh=%v messages=%#v", fresh, runner.Messages())
	}
	if len(a.state.layout.Transcript) != 0 || a.state.editor.HistoryPrevious() {
		t.Fatal("new conversation did not clear transcript and editor history")
	}
	if err := old.AppendEntry(map[string]any{"type": "test"}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("old session was not closed: %v", err)
	}
	if a.state.layout.Session != "" {
		t.Fatalf("unnamed sessions should not clutter the footer: %q", a.state.layout.Session)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAppThemeSwitching(t *testing.T) {
	a := NewApp(AppConfig{})
	a.submit(context.Background(), "/theme dracula")
	want, _ := ThemeByName("dracula")
	if a.state.layout.ThemeName != "dracula" || a.state.layout.Theme != want {
		t.Fatalf("theme = %q %#v", a.state.layout.ThemeName, a.state.layout.Theme)
	}
	a.submit(context.Background(), "/theme missing")
	if entry := a.state.layout.Transcript[len(a.state.layout.Transcript)-1]; !entry.Error {
		t.Fatalf("unknown theme entry = %#v", entry)
	}
}

func TestAppFooterContextIntegration(t *testing.T) {
	a := NewApp(AppConfig{Provider: "test", Model: "model"})
	usage := agent.ContextUsage{Tokens: 32000, ContextWindow: 128000, AutoCompact: true}
	a.handleAgentEvent(agent.Event{Type: "turn_end", ContextUsage: &usage})
	footer := footerText(&a.state.layout, 100)[1]
	if !strings.Contains(footer, "25.0%/128k (auto)") {
		t.Fatalf("footer = %q", footer)
	}
}

func TestTranscriptFromMessages(t *testing.T) {
	messages := []model.Message{
		model.TextMessage("user", "question"),
		{Role: "assistant", Content: []model.Block{{Type: "thinking", Text: "checked"}, {Type: "text", Text: "answer"}, {Type: "tool_use", Name: "read", Arguments: []byte(`{"path":"x"}`)}}},
		{Role: "user", Content: []model.Block{{Type: "tool_result", Text: "nope", IsError: true}}},
	}
	entries := transcriptFromMessages(messages)
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want 4: %#v", len(entries), entries)
	}
	if entries[0].Kind != KindUser || entries[1].Kind != KindThinking || entries[1].Text != "checked" || entries[2].Kind != KindAssistant || entries[3].Kind != KindTool || !entries[3].Error || entries[3].Detail != `path="x"` || entries[3].Text != "nope" {
		t.Fatalf("unexpected transcript: %#v", entries)
	}
}

func TestFormatToolArguments(t *testing.T) {
	got := formatToolArguments(json.RawMessage(`{"offset":2,"path":"README.md","content":"hidden"}`))
	if got != `path="README.md" offset=2` {
		t.Fatalf("arguments = %q", got)
	}
	got = formatToolArguments(json.RawMessage(`{"command":"make check && make build > out < in","url":"https://example.test?a=1&b=2"}`))
	if strings.Contains(got, `\u0026`) || strings.Contains(got, `\u003e`) || strings.Contains(got, `\u003c`) || !strings.Contains(got, `&&`) {
		t.Fatalf("HTML characters were escaped: %q", got)
	}
	long := formatToolArguments(json.RawMessage(`{"command":"` + strings.Repeat("x", 300) + `"}`))
	if len([]rune(long)) > 180 || !strings.HasSuffix(long, "…") {
		t.Fatalf("long arguments were not compacted: %q", long)
	}
}

func TestAppExecMatchesTerminalHost(t *testing.T) {
	a := NewApp(AppConfig{CWD: t.TempDir()})
	stdout, stderr, exit, err := a.Exec(context.Background(), "sh", []string{"-c", "printf out; printf err >&2; exit 7"})
	if stdout != "out" || stderr != "err" || exit != 7 || err == nil {
		t.Fatalf("Exec = (%q, %q, %d, %v)", stdout, stderr, exit, err)
	}
	if !errors.As(err, new(*exec.ExitError)) {
		t.Fatalf("Exec error type = %T", err)
	}
}

func transcriptText(entries []TranscriptEntry) string {
	var parts []string
	for _, entry := range entries {
		parts = append(parts, entry.Label, entry.Text)
	}
	return strings.Join(parts, "\n")
}

func TestQuestionSelectorSeparatesLabelsDescriptionsAndAvoidsDoubleNumbers(t *testing.T) {
	request := &hostRequest{kind: "select", prompt: "Choose a database", options: []string{"SQLite — Simple local storage", "PostgreSQL — Better concurrency"}, editor: NewEditor()}
	rendered := request.render()
	if !strings.Contains(rendered, "? Choose a database") || !strings.Contains(rendered, "❯ ● SQLite\n    Simple local storage") || strings.Contains(rendered, "1. 1.") || !strings.Contains(rendered, "↑/↓ navigate") {
		t.Fatalf("rendered selector:\n%s", rendered)
	}
}

func TestFreeformPromptExplainsControls(t *testing.T) {
	request := &hostRequest{kind: "input", prompt: "What should we call it?", placeholder: "Project name", editor: NewEditor()}
	rendered := request.render()
	if !strings.Contains(rendered, "? What should we call it?") || !strings.Contains(rendered, "Placeholder: Project name") || !strings.Contains(rendered, "Enter submit") {
		t.Fatalf("rendered input:\n%s", rendered)
	}
}
