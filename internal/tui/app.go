package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/modelregistry"
	"github.com/trobrock/notch/internal/resources"
	"github.com/trobrock/notch/internal/session"
)

const (
	appEventBuffer     = 128
	deltaDelay         = 33 * time.Millisecond
	thinkingFrameDelay = 120 * time.Millisecond
	maxPanelLines      = 100
	maxPanelLineBytes  = 4096
)

// AppConfig describes the terminal and the labels shown in the status line.
type AppConfig struct {
	CWD           string
	Provider      string
	Model         string
	Session       string
	SessionDir    string
	Theme         Theme
	ThemeName     string
	Themes        *ThemeCatalog
	ThinkingLevel string
	GitBranch     string
	In            *os.File
	Out           *os.File
}

// App is the fullscreen, event-driven terminal host. It is safe to pass an App
// to extension loaders before Configure or Run; notifications are retained and
// interactive requests rendezvous with the event loop once it starts.
type App struct {
	cfg AppConfig

	mu             sync.Mutex
	runner         *agent.Agent
	registry       *extension.Registry
	catalog        *resources.Catalog
	commandCache   []CommandSuggestion
	listModels     func(context.Context, string, bool) ([]modelregistry.Entry, error)
	switchModel    func(context.Context, string, string, int) (int, error)
	running        bool
	runDone        chan struct{}
	pending        []appEvent
	currentSession *session.Session
	sessionFactory func() (*session.Session, error)

	events chan appEvent

	// Kept as fields to allow package tests to exercise the loop without opening
	// a real terminal. Production instances use OpenScreen and time.NewTimer.
	openScreen func(*os.File, *os.File) (*Screen, error)
	newTimer   func(time.Duration) *time.Timer

	state appState
}

type appState struct {
	layout LayoutState
	editor *Editor

	activeModel         bool
	activeCommand       bool
	cancel              context.CancelFunc
	assistant           int
	thinking            int
	compaction          int
	tools               map[string]int
	queuedText          map[string]string
	promptErrored       bool
	commandHelp         bool
	completionDismissed string
	exit                bool

	modal      *hostRequest
	modalQueue []*hostRequest
}

type appEvent struct {
	keys       []KeyEvent
	agent      *agent.Event
	promptDone *promptResult
	command    *commandResult
	resumeDone *resumeResult
	modelDone  *modelSelectionResult
	request    *hostRequest
	cancelReq  *hostRequest
	notice     *noticeEvent
	status     *statusEvent
	panel      *panelEvent
	followUp   string
	readErr    error
}

type noticeEvent struct{ message, level string }
type statusEvent struct{ key, value string }
type panelEvent struct {
	key, title string
	lines      []string
}
type promptResult struct{ err error }
type resumeResult struct {
	session *session.Session
	err     error
}
type modelSelectionResult struct {
	provider      string
	model         string
	contextWindow int
	warning       error
	err           error
}
type commandResult struct {
	name, text string
	err        error
}
type hostResponse struct {
	value string
	err   error
}

type hostRequest struct {
	kind        string
	ctx         context.Context
	prompt      string
	placeholder string
	options     []string
	selected    int
	editor      *Editor
	entry       int
	reply       chan hostResponse
}

// NewApp constructs an extension.Host before extensions are loaded.
func NewApp(cfg AppConfig) *App {
	if cfg.In == nil {
		cfg.In = os.Stdin
	}
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	if cfg.CWD == "" {
		if cwd, err := os.Getwd(); err == nil {
			cfg.CWD = cwd
		}
	}
	if cfg.Themes == nil {
		cfg.Themes = BuiltinThemeCatalog()
	}
	if cfg.ThemeName == "" {
		cfg.ThemeName = "dark"
	}
	if cfg.Theme == (Theme{}) {
		if theme, canonical, ok := cfg.Themes.Lookup(cfg.ThemeName); ok {
			cfg.Theme, cfg.ThemeName = theme, canonical
		}
	}
	if cfg.ThinkingLevel == "" {
		cfg.ThinkingLevel = "off"
	}
	editor := NewEditor()
	a := &App{
		cfg:        cfg,
		events:     make(chan appEvent, appEventBuffer),
		openScreen: OpenScreen,
		newTimer:   time.NewTimer,
	}
	a.state = appState{
		editor: editor,
		layout: LayoutState{
			Editor: editor, Provider: cfg.Provider, Model: cfg.Model,
			Session: cfg.Session, Status: "ready", Statuses: make(map[string]string), Panels: make(map[string]ExtensionPanel), CWD: abbreviateHome(cfg.CWD),
			GitBranch: cfg.GitBranch, Theme: cfg.Theme, ThemeName: cfg.ThemeName,
			ThinkingLevel: cfg.ThinkingLevel,
		},
		assistant:  -1,
		thinking:   -1,
		compaction: -1,
		tools:      make(map[string]int),
		queuedText: make(map[string]string),
	}
	return a
}

func abbreviateHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	cleanPath, cleanHome := filepath.Clean(path), filepath.Clean(home)
	if cleanPath == cleanHome {
		return "~"
	}
	if strings.HasPrefix(cleanPath, cleanHome+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(cleanPath, cleanHome)
	}
	return path
}

// Configure supplies the objects which are normally created after extensions
// have already received their Host.
func (a *App) SetSession(path string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Session = path
	a.state.layout.Session = ""
}

// SetModelManager enables runtime provider/model discovery and switching.
func (a *App) SetModelManager(list func(context.Context, string, bool) ([]modelregistry.Entry, error), switcher func(context.Context, string, string, int) (int, error)) {
	a.mu.Lock()
	a.listModels, a.switchModel = list, switcher
	a.mu.Unlock()
}

// SetSessionFactory enables /new to create and install a distinct session.
// current remains owned by App and is closed when replaced or when Run exits.
func (a *App) SetSessionFactory(current *session.Session, factory func() (*session.Session, error)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.currentSession = current
	a.sessionFactory = factory
	if current != nil {
		a.cfg.Session = current.Path()
		a.state.layout.Session = ""
	}
}

func (a *App) Configure(runner *agent.Agent, registry *extension.Registry, catalog *resources.Catalog) {
	a.mu.Lock()
	a.runner, a.registry, a.catalog = runner, registry, catalog
	a.commandCache = nil
	a.mu.Unlock()

	a.state.layout.Transcript = transcriptFromMessages(runnerMessages(runner))
	a.state.assistant = -1
	a.state.thinking = -1
	a.state.compaction = -1
	a.state.tools = make(map[string]int)
	if runner != nil {
		a.state.layout.ThinkingLevel = runner.ThinkingLevel()
		a.applyContextUsage(runner.ContextUsage())
	}
}

func runnerMessages(runner *agent.Agent) []model.Message {
	if runner == nil {
		return nil
	}
	return runner.Messages()
}

// Run enters raw alternate-screen mode and processes input, resize, model, and
// extension-host events. It has no polling or idle render timer.
func (a *App) Run(ctx context.Context) (retErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return errors.New("tui: app is already running")
	}
	if a.runner == nil {
		a.mu.Unlock()
		return errors.New("tui: app is not configured with an agent")
	}
	a.running = true
	a.runDone = make(chan struct{})
	pending := append([]appEvent(nil), a.pending...)
	a.pending = nil
	runDone := a.runDone
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		if a.running && a.runDone == runDone {
			a.running = false
			close(runDone)
		}
		a.mu.Unlock()
	}()
	defer func() { retErr = errors.Join(retErr, a.closeCurrentSession()) }()

	screen, err := a.openScreen(a.cfg.In, a.cfg.Out)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, screen.Close()) }()

	width, height, err := screen.Size()
	if err != nil {
		return err
	}
	a.state.layout.Width, a.state.layout.Height = width, height
	for _, event := range pending {
		a.applyEvent(ctx, event)
	}
	if err := screen.Render(BuildFrame(&a.state.layout)); err != nil {
		return err
	}

	inputCtx, stopInput := context.WithCancel(ctx)
	defer stopInput()
	go a.readInput(inputCtx)

	resize := make(chan os.Signal, 1)
	signalNotify(resize)
	defer signalStop(resize)

	var delta strings.Builder
	deltaEntry := -1
	deltaType := ""
	var deltaTimer *time.Timer
	var deltaC <-chan time.Time
	stopDeltaTimer := func() {
		if deltaTimer != nil && !deltaTimer.Stop() {
			select {
			case <-deltaTimer.C:
			default:
			}
		}
		deltaTimer, deltaC = nil, nil
	}
	flushDelta := func(timerFired bool) bool {
		if delta.Len() == 0 {
			if !timerFired {
				stopDeltaTimer()
			} else {
				deltaTimer, deltaC = nil, nil
			}
			return false
		}
		if !timerFired {
			stopDeltaTimer()
		} else {
			deltaTimer, deltaC = nil, nil
		}
		changed := a.appendDelta(deltaEntry, delta.String())
		delta.Reset()
		deltaEntry = -1
		deltaType = ""
		return changed
	}
	defer stopDeltaTimer()

	var thinkingTimer *time.Timer
	var thinkingC <-chan time.Time
	stopThinkingTimer := func() {
		if thinkingTimer != nil && !thinkingTimer.Stop() {
			select {
			case <-thinkingTimer.C:
			default:
			}
		}
		thinkingTimer, thinkingC = nil, nil
	}
	defer stopThinkingTimer()

	for {
		if a.pendingThinkingIndicator() {
			if thinkingTimer == nil {
				thinkingTimer = a.newTimer(thinkingFrameDelay)
				thinkingC = thinkingTimer.C
			}
		} else {
			stopThinkingTimer()
		}
		dirty := false
		select {
		case <-ctx.Done():
			if a.state.cancel != nil {
				a.state.cancel()
			}
			a.cancelHostRequests(ctx.Err())
			return ctx.Err()
		case <-resize:
			w, h, sizeErr := screen.Size()
			if sizeErr != nil {
				return sizeErr
			}
			if w != a.state.layout.Width || h != a.state.layout.Height {
				a.state.layout.Width, a.state.layout.Height = w, h
				dirty = true
			}
		case <-deltaC:
			dirty = flushDelta(true)
		case <-thinkingC:
			thinkingTimer, thinkingC = nil, nil
			a.state.layout.ThinkingFrame++
			dirty = true
		case event := <-a.events:
			isStreamDelta := event.agent != nil && (event.agent.Type == "text_delta" || event.agent.Type == "thinking_delta")
			if isStreamDelta {
				if delta.Len() != 0 && deltaType != event.agent.Type {
					dirty = flushDelta(false) || dirty
				}
				if delta.Len() == 0 {
					deltaEntry = a.streamDeltaEntry(event.agent.Type)
					deltaType = event.agent.Type
					deltaTimer = a.newTimer(deltaDelay)
					deltaC = deltaTimer.C
				}
				delta.WriteString(event.agent.Text)
			} else {
				// Preserve model event ordering: a turn/tool/end event observes all
				// streamed content which preceded it, even if the coalescing delay has not elapsed.
				if event.agent != nil || event.promptDone != nil {
					dirty = flushDelta(false) || dirty
				}
				dirty = a.applyEvent(ctx, event) || dirty
			}
		}

		if a.state.exit {
			if a.state.cancel != nil {
				a.state.cancel()
			}
			a.cancelHostRequests(context.Canceled)
			return nil
		}
		if dirty {
			if err := screen.Render(BuildFrame(&a.state.layout)); err != nil {
				return err
			}
		}
	}
}

// This variable keeps signal cleanup replaceable in event-loop tests.
var signalStop = func(ch chan<- os.Signal) { signal.Stop(ch) }

func (a *App) readInput(ctx context.Context) {
	parser := NewParser()
	buffer := make([]byte, 4096)
	for {
		n, err := a.cfg.In.Read(buffer)
		// A blocking read may return after this particular Run has ended. Do not
		// let that old reader feed a later Run of the same App.
		if ctx.Err() != nil {
			return
		}
		if n > 0 {
			keys := parser.Feed(buffer[:n])
			// A raw terminal commonly returns a literal Escape as one read. Avoid a
			// polling deadline while still supporting Escape cancellation.
			if n == 1 && buffer[0] == 0x1b && len(keys) == 0 {
				keys = parser.FlushEscape()
			}
			if len(keys) != 0 && !a.post(ctx, appEvent{keys: keys}, false) {
				return
			}
		}
		if err != nil {
			a.post(ctx, appEvent{readErr: err}, false)
			return
		}
	}
}

func (a *App) applyEvent(runCtx context.Context, event appEvent) bool {
	changed := false
	for _, key := range event.keys {
		keyChanged, exit := a.handleKey(runCtx, key)
		changed = changed || keyChanged
		if exit {
			a.state.exit = true
			break
		}
	}
	if event.agent != nil {
		changed = a.handleAgentEvent(*event.agent) || changed
	}
	if event.promptDone != nil {
		changed = a.finishPrompt(event.promptDone.err) || changed
	}
	if event.command != nil {
		changed = a.finishCommand(*event.command) || changed
	}
	if event.resumeDone != nil {
		changed = a.finishResume(*event.resumeDone) || changed
	}
	if event.modelDone != nil {
		changed = a.finishModelSelection(*event.modelDone) || changed
	}
	if event.notice != nil {
		a.addNotice(event.notice.message, event.notice.level)
		changed = true
	}
	if event.status != nil {
		if event.status.value == "" {
			delete(a.state.layout.Statuses, event.status.key)
		} else {
			a.state.layout.Statuses[event.status.key] = event.status.value
		}
		changed = true
	}
	if event.panel != nil {
		if event.panel.title == "" && len(event.panel.lines) == 0 {
			delete(a.state.layout.Panels, event.panel.key)
		} else {
			a.state.layout.Panels[event.panel.key] = ExtensionPanel{Key: event.panel.key, Title: event.panel.title, Lines: append([]string(nil), event.panel.lines...)}
		}
		changed = true
	}
	if event.followUp != "" {
		if a.state.activeModel {
			if queued, err := a.runner.FollowUp(event.followUp); err == nil {
				if a.state.queuedText == nil {
					a.state.queuedText = make(map[string]string)
				}
				a.state.queuedText[queued.ID] = event.followUp
				a.state.layout.PendingMessages = append(a.state.layout.PendingMessages, PendingMessage{ID: queued.ID, Mode: queued.Mode, Text: event.followUp})
			} else {
				a.addNotice(err.Error(), "error")
			}
		} else {
			changed = a.submit(runCtx, event.followUp) || changed
		}
		changed = true
	}
	if event.request != nil {
		changed = a.enqueueHostRequest(event.request) || changed
	}
	if event.cancelReq != nil {
		changed = a.cancelHostRequest(event.cancelReq, event.cancelReq.ctx.Err()) || changed
	}
	if event.readErr != nil {
		if errors.Is(event.readErr, io.EOF) {
			if a.activeEditor().Text() == "" && !a.state.activeModel && !a.state.activeCommand {
				a.state.exit = true
			}
		} else {
			a.addNotice(event.readErr.Error(), "error")
			changed = true
		}
	}
	return changed
}

func (a *App) handleKey(runCtx context.Context, key KeyEvent) (changed, exit bool) {
	if a.state.modal != nil {
		return a.handleModalKey(key)
	}
	defer func() {
		if changed {
			a.refreshCommandCompletion()
		}
	}()
	e := a.state.editor
	if key.Text != "" {
		a.state.commandHelp = false
		a.state.completionDismissed = ""
		e.Insert(key.Text)
		a.state.layout.ScrollOffset = 0
		return true, false
	}
	switch key.Key {
	case KeyEnter, KeyAltEnter:
		if len(a.state.layout.CommandSuggestions) != 0 && (a.state.commandHelp || !a.completionIsExact()) {
			return a.acceptCommandCompletion(), false
		}
		if a.state.activeModel {
			mode := "steer"
			if key.Key == KeyAltEnter {
				mode = "follow_up"
			}
			return a.queueComposerMessage(mode), false
		}
		if a.state.activeCommand {
			return false, false
		}
		text := e.Text()
		if strings.TrimSpace(text) == "" {
			return false, false
		}
		e.AddHistory(text)
		e.Clear()
		return a.submit(runCtx, text), a.state.exit
	case KeyNewline:
		e.Insert("\n")
		return true, false
	case KeyTab:
		if len(a.state.layout.CommandSuggestions) != 0 {
			return a.acceptCommandCompletion(), false
		}
		e.Insert("\t")
		return true, false
	case KeyShiftTab:
		a.cycleThinkingLevel()
		return true, false
	case KeyBackspace:
		return e.Backspace(), false
	case KeyDelete:
		return e.Delete(), false
	case KeyLeft, KeyCtrlB:
		return e.MoveLeft(), false
	case KeyRight, KeyCtrlF:
		return e.MoveRight(), false
	case KeyAltLeft:
		return e.MoveWordLeft(), false
	case KeyAltRight:
		return e.MoveWordRight(), false
	case KeyHome, KeyCtrlA:
		return e.MoveHome(), false
	case KeyEnd, KeyCtrlE:
		return e.MoveEnd(), false
	case KeyUp:
		if len(a.state.layout.CommandSuggestions) != 0 {
			a.state.layout.CommandSelection = (a.state.layout.CommandSelection - 1 + len(a.state.layout.CommandSuggestions)) % len(a.state.layout.CommandSuggestions)
			return true, false
		}
		if e.MoveUp() {
			return true, false
		}
		return e.HistoryPrevious(), false
	case KeyDown:
		if len(a.state.layout.CommandSuggestions) != 0 {
			a.state.layout.CommandSelection = (a.state.layout.CommandSelection + 1) % len(a.state.layout.CommandSuggestions)
			return true, false
		}
		if e.MoveDown() {
			return true, false
		}
		return e.HistoryNext(), false
	case KeyCtrlK:
		return e.KillToEnd() != "", false
	case KeyCtrlU:
		return e.KillToStart() != "", false
	case KeyCtrlW:
		return e.KillWordBackward() != "", false
	case KeyPageUp:
		a.state.layout.ScrollOffset += max(1, a.state.layout.Height-4)
		return true, false
	case KeyPageDown:
		old := a.state.layout.ScrollOffset
		a.state.layout.ScrollOffset -= max(1, a.state.layout.Height-4)
		if a.state.layout.ScrollOffset < 0 {
			a.state.layout.ScrollOffset = 0
		}
		return old != a.state.layout.ScrollOffset, false
	case KeyEscape:
		if len(a.state.layout.CommandSuggestions) != 0 {
			a.state.commandHelp = false
			a.state.completionDismissed = e.Text()
			a.state.layout.CommandSuggestions = nil
			return true, false
		}
	case KeyCtrlC:
		if a.state.activeModel || a.state.activeCommand {
			if a.state.cancel != nil {
				a.state.cancel()
			}
			a.state.layout.Status = "canceling"
			return true, false
		}
		if e.Text() != "" {
			e.Clear()
			return true, false
		}
		return false, true
	case KeyCtrlD:
		if e.Text() == "" {
			return false, true
		}
		return e.Delete(), false
	}
	return false, false
}

func (a *App) queueComposerMessage(mode string) bool {
	text := a.state.editor.Text()
	if strings.TrimSpace(text) == "" {
		return false
	}
	expanded := text
	if a.catalog != nil {
		value, err := a.catalog.ExpandInput(text)
		if err == nil {
			expanded = value
		} else if strings.HasPrefix(strings.TrimSpace(text), "/") {
			a.addNotice("cannot queue command while streaming: "+err.Error(), "error")
			return true
		}
	}
	a.mu.Lock()
	runner := a.runner
	a.mu.Unlock()
	if runner == nil {
		a.addNotice("message queue is unavailable", "error")
		return true
	}
	var queued agent.QueuedMessage
	var err error
	if mode == "follow_up" {
		queued, err = runner.FollowUp(expanded)
	} else {
		queued, err = runner.Steer(expanded)
	}
	if err != nil {
		a.addNotice("queue message: "+err.Error(), "error")
		return true
	}
	a.state.editor.AddHistory(text)
	a.state.editor.Clear()
	if a.state.queuedText == nil {
		a.state.queuedText = make(map[string]string)
	}
	a.state.queuedText[queued.ID] = text
	a.state.layout.PendingMessages = append(a.state.layout.PendingMessages, PendingMessage{ID: queued.ID, Mode: queued.Mode, Text: text})
	a.state.layout.ScrollOffset = 0
	return true
}

func (a *App) submit(runCtx context.Context, input string) bool {
	trimmed := strings.TrimSpace(input)
	if strings.HasPrefix(trimmed, "/") {
		name, args := slashParts(trimmed)
		switch name {
		case "exit", "quit":
			a.state.exit = true
			return true
		case "clear":
			a.state.layout.Transcript = nil
			a.state.layout.ScrollOffset = 0
			a.state.assistant, a.state.thinking, a.state.compaction = -1, -1, -1
			a.state.tools = make(map[string]int)
			return true
		case "thinking":
			a.setThinkingLevel(args)
			return true
		case "compact":
			a.startCompact(runCtx, args)
			return true
		case "new":
			a.newConversation()
			return true
		case "resume":
			a.startResume(runCtx)
			return true
		case "model", "provider", "models":
			a.startModelSelection(runCtx, strings.EqualFold(strings.TrimSpace(args), "refresh"))
			return true
		case "theme":
			a.setTheme(args)
			return true
		case "help":
			a.showHelp()
			return true
		case "tools":
			a.showTools()
			return true
		case "skills":
			a.showSkills()
			return true
		}
		if command, ok := a.extensionCommand(name); ok {
			a.startCommand(runCtx, name, args, command)
			return true
		}
	}

	expanded := input
	if a.catalog != nil {
		value, err := a.catalog.ExpandInput(input)
		if err != nil && strings.HasPrefix(trimmed, "/") {
			a.addNotice(err.Error(), "error")
			return true
		}
		if err == nil {
			expanded = value
		}
	} else if strings.HasPrefix(trimmed, "/") {
		a.addNotice("unknown command "+strings.Fields(trimmed)[0], "error")
		return true
	}

	a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: KindUser, Text: input})
	a.state.layout.ScrollOffset = 0
	a.state.layout.Status = "thinking"
	a.state.activeModel = true
	a.state.promptErrored = false
	a.state.assistant, a.state.thinking = -1, -1
	promptCtx, cancel := context.WithCancel(runCtx)
	a.state.cancel = cancel

	a.mu.Lock()
	runner := a.runner
	a.mu.Unlock()
	go func() {
		err := runner.Prompt(promptCtx, expanded, a.agentEventPoster(promptCtx))
		// Prompt cancellation must not suppress its completion event.
		a.post(runCtx, appEvent{promptDone: &promptResult{err: err}}, false)
	}()
	return true
}

func (a *App) agentEventPoster(ctx context.Context) func(agent.Event) {
	return func(agentEvent agent.Event) {
		event := agentEvent
		a.post(ctx, appEvent{agent: &event}, false)
	}
}

func (a *App) finishPrompt(err error) bool {
	if a.state.cancel != nil {
		a.state.cancel()
	}
	a.state.cancel = nil
	a.state.activeModel = false
	if a.state.assistant >= 0 && a.state.assistant < len(a.state.layout.Transcript) {
		a.state.layout.Transcript[a.state.assistant].Pending = false
	}
	a.finishThinking(true)
	a.state.assistant = -1
	a.state.layout.Status = "ready"
	compactionHandled := err != nil && a.finishCompactionError(err)
	if err != nil && errors.Is(err, context.Canceled) && !compactionHandled {
		a.addNotice("request canceled", "notice")
	} else if err != nil && !a.state.promptErrored && !compactionHandled {
		a.addNotice(err.Error(), "error")
	}
	return true
}

func (a *App) handleAgentEvent(event agent.Event) bool {
	switch event.Type {
	case "turn_start":
		a.state.assistant, a.state.thinking = -1, -1
		a.state.layout.ThinkingFrame = 0
		if a.state.layout.ThinkingLevel != "off" {
			a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: KindThinking, Pending: true})
			a.state.thinking = len(a.state.layout.Transcript) - 1
		}
		a.state.layout.Status = "thinking"
	case "queue_update":
		a.state.layout.PendingMessages = make([]PendingMessage, 0, len(event.Queue))
		for _, queued := range event.Queue {
			text := queued.Text
			if display := a.state.queuedText[queued.ID]; display != "" {
				text = display
			}
			a.state.layout.PendingMessages = append(a.state.layout.PendingMessages, PendingMessage{ID: queued.ID, Mode: queued.Mode, Text: text})
		}
	case "queue_delivered":
		if event.Queued == nil {
			return false
		}
		text := event.Queued.Text
		if display := a.state.queuedText[event.Queued.ID]; display != "" {
			text = display
		}
		delete(a.state.queuedText, event.Queued.ID)
		a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: KindUser, Text: text, Label: event.Queued.Mode})
	case "thinking_delta":
		return a.appendDelta(a.ensureThinking(), event.Text)
	case "text_delta":
		a.finishThinking(true)
		a.state.layout.Status = "streaming"
		return a.appendDelta(a.ensureAssistant(), event.Text)
	case "turn_end":
		a.finishThinking(true)
		if i := a.state.assistant; i >= 0 && i < len(a.state.layout.Transcript) {
			if a.state.layout.Transcript[i].Text == "" {
				a.state.layout.Transcript = append(a.state.layout.Transcript[:i], a.state.layout.Transcript[i+1:]...)
				a.state.assistant = -1
			} else {
				a.state.layout.Transcript[i].Pending = false
			}
		}
		if event.Usage != nil {
			a.state.layout.Usage = fmt.Sprintf("%d in / %d out", event.Usage.InputTokens, event.Usage.OutputTokens)
		}
		if event.ContextUsage != nil {
			a.applyContextUsage(*event.ContextUsage)
		}
		a.state.layout.Status = "working"
	case "compaction_start":
		mode := "manually"
		if event.Auto {
			mode = "automatically"
		}
		a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{
			Kind: KindNotice, Label: "compact", Text: "Compacting context " + mode, Pending: true,
		})
		a.state.compaction = len(a.state.layout.Transcript) - 1
		if event.ContextUsage != nil {
			a.applyContextUsage(*event.ContextUsage)
		}
		a.state.layout.Status = "compacting"
	case "compaction_end":
		i := a.state.compaction
		if i < 0 || i >= len(a.state.layout.Transcript) {
			a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: KindNotice, Label: "compact"})
			i = len(a.state.layout.Transcript) - 1
			a.state.compaction = i
		}
		mode := "manually"
		if event.Auto {
			mode = "automatically"
		}
		a.state.layout.Transcript[i].Text = "Context compacted " + mode
		a.state.layout.Transcript[i].Pending = false
		if event.ContextUsage != nil {
			a.applyContextUsage(*event.ContextUsage)
		}
		a.state.layout.Status = "working"
	case "tool_start":
		entry := TranscriptEntry{Kind: KindTool, Label: event.ToolName, Detail: formatToolArguments(event.Arguments), Pending: true}
		a.state.layout.Transcript = append(a.state.layout.Transcript, entry)
		a.state.tools[event.ToolCallID] = len(a.state.layout.Transcript) - 1
		a.state.layout.Status = "tool: " + event.ToolName
	case "tool_update":
		i := a.toolEntry(event)
		entry := &a.state.layout.Transcript[i]
		if entry.Text == "" {
			entry.Text = event.Text
		} else if event.Text != "" {
			entry.Text += "\n" + event.Text
		}
		entry.Text = compactToolText(entry.Text, 8)
	case "tool_end":
		i := a.toolEntry(event)
		entry := &a.state.layout.Transcript[i]
		entry.Pending = false
		if event.Result != nil {
			limit := 8
			if event.Result.IsError {
				limit = 16
			}
			entry.Text = compactToolText(event.Result.Content, limit)
			entry.Error, entry.IsError = event.Result.IsError, event.Result.IsError
		} else if entry.Text == "" {
			entry.Text = "done"
		}
		a.state.layout.Status = "working"
	case "error":
		a.addNotice(event.Text, "error")
		a.state.promptErrored = true
		a.state.layout.Status = "error"
	default:
		return false
	}
	a.state.layout.ScrollOffset = 0
	return true
}

func (a *App) pendingThinkingIndicator() bool {
	i := a.state.thinking
	return i >= 0 && i < len(a.state.layout.Transcript) &&
		a.state.layout.Transcript[i].Kind == KindThinking &&
		a.state.layout.Transcript[i].Pending &&
		strings.TrimSpace(a.state.layout.Transcript[i].Text) == ""
}

func (a *App) streamDeltaEntry(eventType string) int {
	if eventType == "thinking_delta" {
		return a.ensureThinking()
	}
	a.finishThinking(true)
	a.state.layout.Status = "streaming"
	return a.ensureAssistant()
}

func (a *App) ensureThinking() int {
	i := a.state.thinking
	if i >= 0 && i < len(a.state.layout.Transcript) && a.state.layout.Transcript[i].Kind == KindThinking {
		return i
	}
	a.state.layout.ThinkingFrame = 0
	a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: KindThinking, Pending: true})
	a.state.thinking = len(a.state.layout.Transcript) - 1
	return a.state.thinking
}

func (a *App) finishThinking(removeEmpty bool) {
	i := a.state.thinking
	if i < 0 || i >= len(a.state.layout.Transcript) || a.state.layout.Transcript[i].Kind != KindThinking {
		a.state.thinking = -1
		return
	}
	entry := &a.state.layout.Transcript[i]
	entry.Pending = false
	if removeEmpty && strings.TrimSpace(entry.Text) == "" {
		a.removeTranscriptEntry(i)
	}
	a.state.thinking = -1
}

func (a *App) removeTranscriptEntry(index int) {
	if index < 0 || index >= len(a.state.layout.Transcript) {
		return
	}
	a.state.layout.Transcript = append(a.state.layout.Transcript[:index], a.state.layout.Transcript[index+1:]...)
	adjust := func(value *int) {
		if *value == index {
			*value = -1
		} else if *value > index {
			*value--
		}
	}
	adjust(&a.state.assistant)
	adjust(&a.state.thinking)
	adjust(&a.state.compaction)
	for id, value := range a.state.tools {
		if value == index {
			delete(a.state.tools, id)
		} else if value > index {
			a.state.tools[id] = value - 1
		}
	}
}

func (a *App) ensureAssistant() int {
	i := a.state.assistant
	if i >= 0 && i < len(a.state.layout.Transcript) && a.state.layout.Transcript[i].Kind == KindAssistant {
		return i
	}
	a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: KindAssistant, Pending: true})
	a.state.assistant = len(a.state.layout.Transcript) - 1
	return a.state.assistant
}

func (a *App) appendDelta(index int, text string) bool {
	if text == "" {
		return false
	}
	if index < 0 || index >= len(a.state.layout.Transcript) {
		index = a.ensureAssistant()
	}
	a.state.layout.Transcript[index].Text += text
	a.state.layout.Transcript[index].Pending = true
	a.state.layout.ScrollOffset = 0
	return true
}

func (a *App) toolEntry(event agent.Event) int {
	if i, ok := a.state.tools[event.ToolCallID]; ok && i >= 0 && i < len(a.state.layout.Transcript) {
		return i
	}
	a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{
		Kind: KindTool, Label: event.ToolName, Detail: formatToolArguments(event.Arguments), Pending: true,
	})
	i := len(a.state.layout.Transcript) - 1
	a.state.tools[event.ToolCallID] = i
	return i
}

func slashParts(input string) (name, args string) {
	command, args, _ := strings.Cut(strings.TrimPrefix(input, "/"), " ")
	return command, strings.TrimSpace(args)
}

func (a *App) applyContextUsage(usage agent.ContextUsage) {
	a.state.layout.ContextTokens = usage.Tokens
	a.state.layout.ContextWindow = usage.ContextWindow
	a.state.layout.AutoCompact = usage.AutoCompact
}

func (a *App) cycleThinkingLevel() {
	levels := []string{"off", "minimal", "low", "medium", "high", "xhigh"}
	current := strings.ToLower(strings.TrimSpace(a.state.layout.ThinkingLevel))
	next := levels[0]
	for i, level := range levels {
		if current == level {
			next = levels[(i+1)%len(levels)]
			break
		}
	}
	a.applyThinkingLevel(next, false)
}

func (a *App) setThinkingLevel(level string) {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		current := a.state.layout.ThinkingLevel
		a.mu.Lock()
		runner := a.runner
		a.mu.Unlock()
		if runner != nil {
			current = runner.ThinkingLevel()
			a.state.layout.ThinkingLevel = current
		}
		if current == "" {
			current = "off"
		}
		a.addNotice("thinking level: "+current, "notice")
		return
	}
	a.applyThinkingLevel(level, true)
}

func (a *App) applyThinkingLevel(level string, report bool) {
	a.mu.Lock()
	runner := a.runner
	a.mu.Unlock()
	if runner == nil {
		a.addNotice("thinking level is unavailable", "error")
		return
	}
	if err := runner.SetThinkingLevel(level); err != nil {
		a.addNotice(err.Error(), "error")
		return
	}
	a.cfg.ThinkingLevel = level
	a.state.layout.ThinkingLevel = level
	if report {
		a.addNotice("thinking level: "+level, "notice")
	}
}

func (a *App) setTheme(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		a.addNotice(strings.Join(a.cfg.Themes.Names(), "\n"), "notice")
		return
	}
	theme, canonical, ok := a.cfg.Themes.Lookup(name)
	if !ok {
		a.addNotice(fmt.Sprintf("unknown theme %q", name), "error")
		return
	}
	a.cfg.Theme, a.cfg.ThemeName = theme, canonical
	a.state.layout.Theme, a.state.layout.ThemeName = theme, canonical
	a.addNotice("theme: "+canonical, "notice")
}

func (a *App) startCompact(runCtx context.Context, instructions string) {
	a.mu.Lock()
	runner := a.runner
	a.mu.Unlock()
	if runner == nil {
		a.addNotice("compaction is unavailable", "error")
		return
	}
	commandCtx, cancel := context.WithCancel(runCtx)
	a.state.activeCommand = true
	a.state.cancel = cancel
	a.state.layout.Status = "compacting"
	go func() {
		err := runner.Compact(commandCtx, instructions, false, a.agentEventPoster(commandCtx))
		text := ""
		if errors.Is(err, agent.ErrNothingToCompact) {
			text, err = "nothing to compact", nil
		}
		a.post(runCtx, appEvent{command: &commandResult{name: "compact", text: text, err: err}}, false)
	}()
}

func (a *App) startModelSelection(runCtx context.Context, force bool) {
	if a.state.activeModel || a.state.activeCommand {
		a.addNotice("cannot change model while work is running", "error")
		return
	}
	a.mu.Lock()
	list, switcher := a.listModels, a.switchModel
	a.mu.Unlock()
	if list == nil || switcher == nil {
		a.addNotice("model selection is unavailable", "error")
		return
	}
	commandCtx, cancel := context.WithCancel(runCtx)
	a.state.activeCommand = true
	a.state.cancel = cancel
	a.state.layout.Status = "models"
	currentProvider, currentModel := a.state.layout.Provider, a.state.layout.Model
	go func() {
		providers := modelregistry.Providers()
		for i, provider := range providers {
			if provider == currentProvider && i != 0 {
				providers[0], providers[i] = providers[i], providers[0]
				break
			}
		}
		providerName, err := a.Select(commandCtx, "Select provider", providers)
		if err != nil {
			a.post(runCtx, appEvent{modelDone: &modelSelectionResult{err: err}}, false)
			return
		}
		models, warning := list(commandCtx, providerName, force)
		if len(models) == 0 {
			if warning == nil {
				warning = errors.New("no models available")
			}
			a.post(runCtx, appEvent{modelDone: &modelSelectionResult{err: warning}}, false)
			return
		}
		if providerName == currentProvider {
			found := false
			for _, candidate := range models {
				if candidate.ID == currentModel {
					found = true
					break
				}
			}
			if !found {
				models = append([]modelregistry.Entry{{Provider: providerName, ID: currentModel, Name: currentModel, Source: "current"}}, models...)
			}
			for i, candidate := range models {
				if candidate.ID == currentModel && i != 0 {
					models[0], models[i] = models[i], models[0]
					break
				}
			}
		}
		choices := make([]string, 0, len(models))
		byLabel := make(map[string]modelregistry.Entry, len(models))
		for _, candidate := range models {
			label := modelSelectionLabel(candidate)
			choices = append(choices, label)
			byLabel[label] = candidate
		}
		choice, err := a.Select(commandCtx, "Select model (type to filter)", choices)
		if err != nil {
			a.post(runCtx, appEvent{modelDone: &modelSelectionResult{err: err}}, false)
			return
		}
		selected, ok := byLabel[choice]
		if !ok {
			a.post(runCtx, appEvent{modelDone: &modelSelectionResult{err: errors.New("selected model is unavailable")}}, false)
			return
		}
		actualWindow, err := switcher(commandCtx, providerName, selected.ID, selected.ContextWindow)
		if err != nil {
			a.post(runCtx, appEvent{modelDone: &modelSelectionResult{err: err}}, false)
			return
		}
		a.post(runCtx, appEvent{modelDone: &modelSelectionResult{provider: providerName, model: selected.ID, contextWindow: actualWindow, warning: warning}}, false)
	}()
}

func modelSelectionLabel(entry modelregistry.Entry) string {
	label := entry.ID
	if entry.Name != "" && entry.Name != entry.ID {
		label += " — " + entry.Name
	}
	var details []string
	if entry.ContextWindow > 0 {
		details = append(details, formatTokens(entry.ContextWindow)+" ctx")
	}
	if entry.Reasoning {
		details = append(details, "reasoning")
	}
	if len(details) != 0 {
		label += "  [" + strings.Join(details, ", ") + "]"
	}
	return label
}

func (a *App) finishModelSelection(result modelSelectionResult) bool {
	if a.state.cancel != nil {
		a.state.cancel()
	}
	a.state.cancel = nil
	a.state.activeCommand = false
	a.state.layout.Status = "ready"
	if result.err != nil {
		if !errors.Is(result.err, context.Canceled) {
			a.addNotice("select model: "+result.err.Error(), "error")
		}
		return true
	}
	a.state.layout.Provider, a.state.layout.Model = result.provider, result.model
	a.cfg.Provider, a.cfg.Model = result.provider, result.model
	if result.contextWindow > 0 {
		a.state.layout.ContextWindow = result.contextWindow
	}
	a.addNotice("model: ("+result.provider+") "+result.model, "notice")
	if result.warning != nil {
		a.addNotice("model registry: "+result.warning.Error()+"; showing fallback data", "warning")
	}
	return true
}

func (a *App) startResume(runCtx context.Context) {
	if a.state.activeModel || a.state.activeCommand {
		a.addNotice("cannot resume while work is running", "error")
		return
	}
	if strings.TrimSpace(a.cfg.SessionDir) == "" {
		a.addNotice("session resume is unavailable", "error")
		return
	}
	commandCtx, cancel := context.WithCancel(runCtx)
	a.state.activeCommand = true
	a.state.cancel = cancel
	a.state.layout.Status = "sessions"
	go func() {
		infos, err := session.List(a.cfg.SessionDir)
		if err != nil {
			a.post(runCtx, appEvent{resumeDone: &resumeResult{err: err}}, false)
			return
		}
		a.mu.Lock()
		current := a.currentSession
		a.mu.Unlock()
		choices := make([]string, 0, len(infos))
		byLabel := make(map[string]session.Info, len(infos))
		for _, info := range infos {
			if current != nil && filepath.Clean(info.Path) == filepath.Clean(current.Path()) {
				continue
			}
			label := resumeSessionLabel(info)
			choices = append(choices, label)
			byLabel[label] = info
		}
		if len(choices) == 0 {
			a.post(runCtx, appEvent{resumeDone: &resumeResult{err: errors.New("no other saved sessions")}}, false)
			return
		}
		choice, err := a.Select(commandCtx, "Resume session", choices)
		if err != nil {
			a.post(runCtx, appEvent{resumeDone: &resumeResult{err: err}}, false)
			return
		}
		info, ok := byLabel[choice]
		if !ok {
			a.post(runCtx, appEvent{resumeDone: &resumeResult{err: errors.New("selected session is unavailable")}}, false)
			return
		}
		loaded, err := session.Load(info.Path)
		a.post(runCtx, appEvent{resumeDone: &resumeResult{session: loaded, err: err}}, false)
	}()
}

func resumeSessionLabel(info session.Info) string {
	when := info.ModifiedAt.Local().Format("Jan 02 15:04")
	cwd := abbreviateHome(info.Header.CWD)
	id := info.Header.ID
	if len(id) > 8 {
		id = id[len(id)-8:]
	}
	return fmt.Sprintf("%s  %s  %s  %s  [%s]", when, cwd, info.Header.Model, info.Preview, id)
}

func (a *App) finishResume(result resumeResult) bool {
	if a.state.cancel != nil {
		a.state.cancel()
	}
	a.state.cancel = nil
	a.state.activeCommand = false
	a.state.layout.Status = "ready"
	if result.err != nil {
		if !errors.Is(result.err, context.Canceled) {
			a.addNotice("resume session: "+result.err.Error(), "error")
		}
		a.state.layout.Status = "ready"
		return true
	}
	if result.session == nil {
		a.addNotice("resume session: loaded session is nil", "error")
		a.state.layout.Status = "ready"
		return true
	}
	a.mu.Lock()
	runner, current := a.runner, a.currentSession
	a.mu.Unlock()
	if runner == nil {
		_ = result.session.Close()
		a.addNotice("resume session: agent is unavailable", "error")
		return true
	}
	old, err := runner.ResumeSession(result.session)
	if err != nil {
		_ = result.session.Close()
		a.addNotice(err.Error(), "error")
		return true
	}
	a.mu.Lock()
	a.currentSession = result.session
	a.cfg.Session = result.session.Path()
	a.mu.Unlock()
	a.resetConversationState()
	messages := runner.Messages()
	a.state.layout.Transcript = transcriptFromMessages(messages)
	a.state.editor.SetHistory(promptHistory(messages))
	a.applyContextUsage(runner.ContextUsage())
	if old == nil {
		old = current
	}
	if old != nil && old != result.session {
		if err := old.Close(); err != nil {
			a.addNotice(err.Error(), "error")
		}
	}
	return true
}

func promptHistory(messages []model.Message) []string {
	var history []string
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		var parts []string
		for _, block := range message.Content {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) != 0 {
			history = append(history, strings.Join(parts, "\n"))
		}
	}
	return history
}

func (a *App) newConversation() {
	a.mu.Lock()
	runner, current, factory := a.runner, a.currentSession, a.sessionFactory
	a.mu.Unlock()
	if runner == nil {
		a.addNotice("conversation reset is unavailable", "error")
		return
	}

	var fresh *session.Session
	if factory != nil {
		var err error
		fresh, err = factory()
		if err != nil {
			a.addNotice("create session: "+err.Error(), "error")
			return
		}
		if fresh == nil {
			a.addNotice("create session: factory returned nil", "error")
			return
		}
	}
	old, err := runner.ResetConversation(fresh)
	if err != nil {
		if fresh != nil && fresh != current {
			_ = fresh.Close()
		}
		a.addNotice(err.Error(), "error")
		return
	}
	if fresh != nil {
		a.mu.Lock()
		a.currentSession = fresh
		a.cfg.Session = fresh.Path()
		a.mu.Unlock()
		a.state.layout.Session = ""
		if old == nil {
			old = current
		}
	}
	a.resetConversationState()
	a.applyContextUsage(runner.ContextUsage())
	if old != nil && old != fresh {
		if err := old.Close(); err != nil {
			a.addNotice(err.Error(), "error")
		}
	}
}

func (a *App) resetConversationState() {
	a.state.layout.Transcript = nil
	a.state.layout.ScrollOffset = 0
	a.state.layout.Usage = ""
	a.state.layout.ContextTokens = 0
	a.state.layout.ContextWindow = 0
	a.state.layout.AutoCompact = false
	a.state.layout.Status = "ready"
	a.state.editor.Clear()
	a.state.editor.SetHistory(nil)
	a.state.assistant, a.state.thinking, a.state.compaction = -1, -1, -1
	a.state.tools = make(map[string]int)
	a.state.queuedText = make(map[string]string)
	a.state.layout.PendingMessages = nil
	a.state.promptErrored = false
	a.state.commandHelp = false
	a.state.completionDismissed = ""
	a.state.layout.CommandSuggestions = nil
	a.state.layout.CommandSelection = 0
}

func (a *App) closeCurrentSession() error {
	a.mu.Lock()
	current := a.currentSession
	a.mu.Unlock()
	if current == nil {
		return nil
	}
	return current.Close()
}

func (a *App) extensionCommand(name string) (extension.Command, bool) {
	a.mu.Lock()
	registry := a.registry
	a.mu.Unlock()
	if registry == nil {
		return extension.Command{}, false
	}
	return registry.Command(name)
}

func (a *App) startCommand(runCtx context.Context, name, args string, command extension.Command) {
	commandCtx, cancel := context.WithCancel(runCtx)
	a.state.activeCommand = true
	a.state.cancel = cancel
	a.state.layout.Status = "/" + name
	go func() {
		text, err := command.Execute(commandCtx, args)
		a.post(runCtx, appEvent{command: &commandResult{name: name, text: text, err: err}}, false)
	}()
}

func (a *App) finishCommand(result commandResult) bool {
	if a.state.cancel != nil {
		a.state.cancel()
	}
	a.state.cancel = nil
	a.state.activeCommand = false
	a.state.layout.Status = "ready"
	if result.text != "" {
		a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: KindNotice, Label: result.name, Text: result.text})
	}
	if result.err != nil {
		if result.name == "compact" && a.finishCompactionError(result.err) {
			return true
		}
		if errors.Is(result.err, context.Canceled) {
			a.addNotice("command canceled", "notice")
		} else {
			a.addNotice(result.err.Error(), "error")
		}
	}
	return true
}

func (a *App) finishCompactionError(err error) bool {
	i := a.state.compaction
	if i < 0 || i >= len(a.state.layout.Transcript) || !a.state.layout.Transcript[i].Pending {
		return false
	}
	entry := &a.state.layout.Transcript[i]
	entry.Pending = false
	if errors.Is(err, context.Canceled) {
		entry.Text = "Compaction canceled"
		return true
	}
	entry.Kind, entry.Error, entry.IsError = KindError, true, true
	entry.Text = "Compaction failed: " + err.Error()
	return true
}

func (a *App) showHelp() {
	a.state.editor.Clear()
	a.state.commandHelp = true
	a.state.completionDismissed = ""
	a.state.layout.CommandSelection = 0
	a.refreshCommandCompletion()
	a.state.layout.Status = "command help"
}

func (a *App) commandSuggestions() []CommandSuggestion {
	a.mu.Lock()
	if a.commandCache != nil {
		cached := a.commandCache
		a.mu.Unlock()
		return cached
	}
	registry := a.registry
	a.mu.Unlock()
	items := []CommandSuggestion{
		{Name: "clear", Description: "clear the transcript"},
		{Name: "compact", ArgumentHint: "[instructions]", Description: "compact conversation context"},
		{Name: "exit", Description: "exit Notch"},
		{Name: "help", Description: "browse available commands"},
		{Name: "model", ArgumentHint: "[refresh]", Description: "select provider and model"},
		{Name: "models", ArgumentHint: "[refresh]", Description: "select provider and model"},
		{Name: "new", Description: "start a new conversation"},
		{Name: "provider", ArgumentHint: "[refresh]", Description: "select provider and model"},
		{Name: "quit", Description: "exit Notch"},
		{Name: "resume", Description: "choose a saved session"},
		{Name: "skills", Description: "list loaded skills"},
		{Name: "theme", ArgumentHint: "[name]", Description: "list or select themes"},
		{Name: "thinking", ArgumentHint: "[level]", Description: "show or set thinking level"},
		{Name: "tools", Description: "list loaded tools"},
	}
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		seen[item.Name] = true
	}
	if a.catalog != nil {
		for name, skill := range a.catalog.Skills {
			command := "skill:" + name
			if !seen[command] {
				items = append(items, CommandSuggestion{Name: command, ArgumentHint: "[arguments]", Description: skill.Description})
				seen[command] = true
			}
		}
		for name, template := range a.catalog.Templates {
			if !seen[name] {
				items = append(items, CommandSuggestion{Name: name, ArgumentHint: template.ArgumentHint, Description: template.Description})
				seen[name] = true
			}
		}
	}
	if registry != nil {
		for _, command := range registry.Commands() {
			if !seen[command.Name] {
				items = append(items, CommandSuggestion{Name: command.Name, Description: command.Description})
				seen[command.Name] = true
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	a.mu.Lock()
	if a.commandCache == nil {
		a.commandCache = items
	}
	cached := a.commandCache
	a.mu.Unlock()
	return cached
}

func (a *App) refreshCommandCompletion() {
	if a.state.modal != nil {
		return
	}
	all := a.commandSuggestions()
	text := a.state.editor.Text()
	if a.state.commandHelp && text == "" {
		a.state.layout.CommandSuggestions = all
		a.state.layout.CommandSelection = clamp(a.state.layout.CommandSelection, 0, max(0, len(all)-1))
		return
	}
	a.state.commandHelp = false
	if a.state.layout.Status == "command help" {
		a.state.layout.Status = "ready"
	}
	if text == a.state.completionDismissed || !strings.HasPrefix(text, "/") {
		a.state.layout.CommandSuggestions = nil
		a.state.layout.CommandSelection = 0
		return
	}
	prefix := strings.TrimPrefix(text, "/")
	if strings.ContainsAny(prefix, " \t\r\n") {
		a.state.layout.CommandSuggestions = nil
		a.state.layout.CommandSelection = 0
		return
	}
	prefix = strings.ToLower(prefix)
	filtered := make([]CommandSuggestion, 0, len(all))
	for _, item := range all {
		if strings.HasPrefix(strings.ToLower(item.Name), prefix) {
			filtered = append(filtered, item)
		}
	}
	a.state.layout.CommandSuggestions = filtered
	a.state.layout.CommandSelection = clamp(a.state.layout.CommandSelection, 0, max(0, len(filtered)-1))
}

func (a *App) completionIsExact() bool {
	text := strings.TrimSpace(strings.TrimPrefix(a.state.editor.Text(), "/"))
	if strings.ContainsAny(text, " \t\r\n") {
		return true
	}
	for _, item := range a.state.layout.CommandSuggestions {
		if item.Name == text {
			return true
		}
	}
	return false
}

func (a *App) acceptCommandCompletion() bool {
	items := a.state.layout.CommandSuggestions
	if len(items) == 0 {
		return false
	}
	index := clamp(a.state.layout.CommandSelection, 0, len(items)-1)
	a.state.editor.SetText("/" + items[index].Name + " ")
	a.state.commandHelp = false
	a.state.completionDismissed = ""
	a.state.layout.CommandSuggestions = nil
	return true
}

func (a *App) showTools() {
	a.mu.Lock()
	registry := a.registry
	a.mu.Unlock()
	var lines []string
	if registry != nil {
		for _, tool := range registry.Tools() {
			line := tool.Definition.Name
			if tool.Definition.Description != "" {
				line += " — " + tool.Definition.Description
			}
			if tool.Source != "" {
				line += " (" + tool.Source + ")"
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		lines = []string{"no tools loaded"}
	}
	a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: KindNotice, Label: "tools", Text: strings.Join(lines, "\n")})
}

func (a *App) showSkills() {
	var lines []string
	if a.catalog != nil {
		names := make([]string, 0, len(a.catalog.Skills))
		for name := range a.catalog.Skills {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			line := "/skill:" + name
			if description := a.catalog.Skills[name].Description; description != "" {
				line += " — " + description
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		lines = []string{"no skills loaded"}
	}
	a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: KindNotice, Label: "skills", Text: strings.Join(lines, "\n")})
}

func (a *App) addNotice(message, level string) {
	kind := KindNotice
	isError := strings.EqualFold(level, "error")
	if isError {
		kind = KindError
	}
	label := level
	if label == "" || label == "info" || label == "notice" {
		label = ""
	}
	a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: kind, Label: label, Text: message, Error: isError})
	a.state.layout.ScrollOffset = 0
}

func transcriptFromMessages(messages []model.Message) []TranscriptEntry {
	var transcript []TranscriptEntry
	toolCalls := make(map[string]int)
	for _, message := range messages {
		var text, thinking []string
		flushThinking := func() {
			if len(thinking) == 0 {
				return
			}
			transcript = append(transcript, TranscriptEntry{Kind: KindThinking, Text: strings.Join(thinking, "\n\n")})
			thinking = nil
		}
		flushText := func() {
			if len(text) == 0 {
				return
			}
			kind := KindAssistant
			if message.Role == "user" {
				kind = KindUser
			}
			transcript = append(transcript, TranscriptEntry{Kind: kind, Text: strings.Join(text, "\n")})
			text = nil
		}
		for _, block := range message.Content {
			switch block.Type {
			case "thinking":
				flushText()
				if strings.TrimSpace(block.Text) != "" {
					thinking = append(thinking, block.Text)
				}
			case "text":
				flushThinking()
				text = append(text, block.Text)
			case "tool_use", "function_call":
				flushText()
				flushThinking()
				transcript = append(transcript, TranscriptEntry{Kind: KindTool, Label: block.Name, Detail: formatToolArguments(block.Arguments), Pending: true})
				toolCalls[block.ID] = len(transcript) - 1
			case "tool_result":
				flushText()
				flushThinking()
				limit := 8
				if block.IsError {
					limit = 16
				}
				if index, ok := toolCalls[block.ToolUseID]; ok && index >= 0 && index < len(transcript) {
					entry := &transcript[index]
					entry.Text = compactToolText(block.Text, limit)
					entry.Pending, entry.Error, entry.IsError = false, block.IsError, block.IsError
				} else {
					transcript = append(transcript, TranscriptEntry{Kind: KindTool, Label: "result", Text: compactToolText(block.Text, limit), Error: block.IsError, IsError: block.IsError})
				}
			}
		}
		flushText()
		flushThinking()
	}
	return transcript
}

func formatToolArguments(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "{}" {
		return ""
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return compactInline(string(raw), 180)
	}
	priority := []string{"command", "path", "pattern", "query", "url", "question"}
	keys := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, key := range priority {
		if _, ok := values[key]; ok {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	var rest []string
	for key := range values {
		if !seen[key] && key != "content" && key != "old_text" && key != "new_text" {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		encoded, _ := json.Marshal(values[key])
		parts = append(parts, key+"="+compactInline(string(encoded), 90))
	}
	return compactInline(strings.Join(parts, "  "), 180)
}

func compactInline(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:max(0, limit-1)]) + "…"
}

func compactToolText(text string, maxLines int) string {
	const maxRunes = 2000
	lines := strings.Split(strings.TrimSpace(text), "\n")
	omittedLines := 0
	if len(lines) > maxLines {
		omittedLines = len(lines) - maxLines
		lines = lines[:maxLines]
	}
	result := strings.Join(lines, "\n")
	runes := []rune(result)
	truncated := false
	if len(runes) > maxRunes {
		result = string(runes[:maxRunes])
		truncated = true
	}
	if omittedLines > 0 {
		result += fmt.Sprintf("\n… %d more lines", omittedLines)
	} else if truncated {
		result += "…"
	}
	if result == "" {
		return "done"
	}
	return result
}

// extension.Host implementation.
func (a *App) CWD() string { return a.cfg.CWD }

func (a *App) Exec(ctx context.Context, command string, args []string) (string, string, int, error) {
	if command == "" {
		return "", "", -1, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = a.cfg.CWD
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return stdout.String(), stderr.String(), exitCode, err
}

func (a *App) Input(ctx context.Context, prompt, placeholder string) (string, error) {
	return a.hostPrompt(ctx, &hostRequest{kind: "input", prompt: prompt, placeholder: placeholder})
}

func (a *App) Select(ctx context.Context, prompt string, options []string) (string, error) {
	if len(options) == 0 {
		return "", errors.New("select requires options")
	}
	return a.hostPrompt(ctx, &hostRequest{kind: "select", prompt: prompt, options: append([]string(nil), options...)})
}

func (a *App) hostPrompt(ctx context.Context, request *hostRequest) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	request.ctx = ctx
	request.reply = make(chan hostResponse, 1)
	if !a.post(ctx, appEvent{request: request}, true) {
		return "", ctx.Err()
	}
	select {
	case response := <-request.reply:
		return response.value, response.err
	case <-ctx.Done():
		a.postRunning(appEvent{cancelReq: request})
		return "", ctx.Err()
	}
}

func (a *App) Notify(message, level string) {
	notice := &noticeEvent{message: message, level: level}
	a.post(context.Background(), appEvent{notice: notice}, true)
}

func (a *App) FollowUp(message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		return errors.New("follow-up message is empty")
	}
	a.post(context.Background(), appEvent{followUp: message}, true)
	return nil
}

// SetStatus publishes a keyed extension status in the footer. Reusing a key
// replaces its value; an empty value removes it.
func (a *App) SetStatus(key, value string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	a.post(context.Background(), appEvent{status: &statusEvent{key: key, value: strings.TrimSpace(value)}}, true)
}

// SetPanel publishes a keyed, non-interactive panel above the composer.
func (a *App) SetPanel(key, title string, lines []string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	if len(lines) > maxPanelLines {
		lines = lines[:maxPanelLines]
	}
	bounded := make([]string, len(lines))
	for i, line := range lines {
		if len(line) > maxPanelLineBytes {
			line = line[:maxPanelLineBytes]
			for !utf8.ValidString(line) {
				line = line[:len(line)-1]
			}
		}
		bounded[i] = line
	}
	a.post(context.Background(), appEvent{panel: &panelEvent{
		key: key, title: strings.TrimSpace(title), lines: bounded,
	}}, true)
}

func (a *App) post(ctx context.Context, event appEvent, retainBeforeRun bool) bool {
	a.mu.Lock()
	if !a.running {
		if retainBeforeRun {
			a.pending = append(a.pending, event)
		}
		a.mu.Unlock()
		return retainBeforeRun
	}
	done := a.runDone
	a.mu.Unlock()
	select {
	case a.events <- event:
		return true
	case <-ctx.Done():
		return false
	case <-done:
		return false
	}
}

func (a *App) postRunning(event appEvent) {
	a.mu.Lock()
	if !a.running {
		a.mu.Unlock()
		return
	}
	done := a.runDone
	a.mu.Unlock()
	select {
	case a.events <- event:
	case <-done:
	}
}

func (a *App) enqueueHostRequest(request *hostRequest) bool {
	if err := request.ctx.Err(); err != nil {
		request.reply <- hostResponse{err: err}
		return false
	}
	if a.state.modal != nil {
		a.state.modalQueue = append(a.state.modalQueue, request)
		return false
	}
	a.beginHostRequest(request)
	return true
}

func (a *App) beginHostRequest(request *hostRequest) {
	request.editor = NewEditor()
	request.entry = len(a.state.layout.Transcript)
	a.state.modal = request
	a.state.layout.Editor = request.editor
	a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{
		Kind: KindNotice, Text: request.render(), Pending: true,
	})
	a.state.layout.ScrollOffset = 0
}

func (request *hostRequest) render() string {
	if request.kind == "input" {
		if request.placeholder != "" {
			return fmt.Sprintf("%s (%s)", request.prompt, request.placeholder)
		}
		return request.prompt
	}
	lines := []string{request.prompt}
	indices := request.filteredOptionIndices()
	if request.editor != nil && request.editor.Text() != "" {
		lines = append(lines, "Filter: "+request.editor.Text())
	}
	if len(indices) == 0 {
		return strings.Join(append(lines, "  no matches"), "\n")
	}
	selectedPosition := 0
	for i, index := range indices {
		if index == request.selected {
			selectedPosition = i
			break
		}
	}
	const visible = 9
	start := max(0, selectedPosition-visible/2)
	if start+visible > len(indices) {
		start = max(0, len(indices)-visible)
	}
	end := min(len(indices), start+visible)
	for position := start; position < end; position++ {
		index := indices[position]
		marker := "  "
		if index == request.selected {
			marker = "› "
		}
		lines = append(lines, fmt.Sprintf("%s%d. %s", marker, position+1, request.options[index]))
	}
	if len(indices) > visible {
		lines = append(lines, fmt.Sprintf("  %d matches", len(indices)))
	}
	return strings.Join(lines, "\n")
}

func (request *hostRequest) filteredOptionIndices() []int {
	query := ""
	if request.editor != nil {
		query = strings.ToLower(strings.TrimSpace(request.editor.Text()))
	}
	indices := make([]int, 0, len(request.options))
	for i, option := range request.options {
		if query == "" || strings.Contains(strings.ToLower(option), query) {
			indices = append(indices, i)
		}
	}
	return indices
}

func (request *hostRequest) normalizeSelection() []int {
	indices := request.filteredOptionIndices()
	for _, index := range indices {
		if index == request.selected {
			return indices
		}
	}
	if len(indices) != 0 {
		request.selected = indices[0]
	} else {
		request.selected = -1
	}
	return indices
}

func (a *App) handleModalKey(key KeyEvent) (bool, bool) {
	request := a.state.modal
	if request == nil {
		return false, false
	}
	if key.Key == KeyCtrlC || key.Key == KeyEscape {
		if a.state.cancel != nil {
			a.state.cancel()
		}
		a.resolveHostRequest(request, "", context.Canceled)
		return true, false
	}
	if request.kind == "select" {
		indices := request.normalizeSelection()
		position := -1
		for i, index := range indices {
			if index == request.selected {
				position = i
				break
			}
		}
		switch key.Key {
		case KeyUp:
			if position > 0 {
				request.selected = indices[position-1]
				a.updateModalEntry()
				return true, false
			}
		case KeyDown:
			if position >= 0 && position+1 < len(indices) {
				request.selected = indices[position+1]
				a.updateModalEntry()
				return true, false
			}
		case KeyEnter:
			if request.selected >= 0 && request.selected < len(request.options) {
				a.resolveHostRequest(request, request.options[request.selected], nil)
				return true, false
			}
		case KeyBackspace:
			if request.editor.Backspace() {
				request.normalizeSelection()
				a.updateModalEntry()
				return true, false
			}
		case KeyCtrlU:
			if request.editor.KillToStart() != "" {
				request.normalizeSelection()
				a.updateModalEntry()
				return true, false
			}
		}
		if key.Text != "" {
			request.editor.Insert(key.Text)
			request.normalizeSelection()
			a.updateModalEntry()
			return true, false
		}
		return false, false
	}

	if key.Key == KeyEnter {
		value := request.editor.Text()
		if value == "" {
			value = request.placeholder
		}
		a.resolveHostRequest(request, value, nil)
		return true, false
	}
	// Reuse the regular editing behavior without allowing submission, history,
	// scrolling, or application exit from an extension input rendezvous.
	e := request.editor
	if key.Text != "" {
		e.Insert(key.Text)
		return true, false
	}
	switch key.Key {
	case KeyNewline:
		e.Insert("\n")
		return true, false
	case KeyTab:
		e.Insert("\t")
		return true, false
	case KeyBackspace:
		return e.Backspace(), false
	case KeyDelete, KeyCtrlD:
		return e.Delete(), false
	case KeyLeft, KeyCtrlB:
		return e.MoveLeft(), false
	case KeyRight, KeyCtrlF:
		return e.MoveRight(), false
	case KeyAltLeft:
		return e.MoveWordLeft(), false
	case KeyAltRight:
		return e.MoveWordRight(), false
	case KeyHome, KeyCtrlA:
		return e.MoveHome(), false
	case KeyEnd, KeyCtrlE:
		return e.MoveEnd(), false
	case KeyUp:
		return e.MoveUp(), false
	case KeyDown:
		return e.MoveDown(), false
	case KeyCtrlK:
		return e.KillToEnd() != "", false
	case KeyCtrlU:
		return e.KillToStart() != "", false
	case KeyCtrlW:
		return e.KillWordBackward() != "", false
	}
	return false, false
}

func (a *App) updateModalEntry() {
	request := a.state.modal
	if request != nil && request.entry >= 0 && request.entry < len(a.state.layout.Transcript) {
		a.state.layout.Transcript[request.entry].Text = request.render()
	}
}

func (a *App) resolveHostRequest(request *hostRequest, value string, err error) {
	if request != a.state.modal {
		return
	}
	if request.entry >= 0 && request.entry < len(a.state.layout.Transcript) {
		entry := &a.state.layout.Transcript[request.entry]
		entry.Pending = false
		if err != nil {
			entry.Text += "\n(canceled)"
		}
	}
	if err == nil {
		label := "input"
		if request.kind == "select" {
			label = "selection"
		}
		a.state.layout.Transcript = append(a.state.layout.Transcript, TranscriptEntry{Kind: KindUser, Label: label, Text: value})
	}
	select {
	case request.reply <- hostResponse{value: value, err: err}:
	default:
	}
	a.state.modal = nil
	a.state.layout.Editor = a.state.editor
	a.startNextHostRequest()
}

func (a *App) startNextHostRequest() {
	for len(a.state.modalQueue) != 0 {
		next := a.state.modalQueue[0]
		a.state.modalQueue = a.state.modalQueue[1:]
		if err := next.ctx.Err(); err != nil {
			select {
			case next.reply <- hostResponse{err: err}:
			default:
			}
			continue
		}
		a.beginHostRequest(next)
		return
	}
}

func (a *App) cancelHostRequest(request *hostRequest, err error) bool {
	if err == nil {
		err = context.Canceled
	}
	if request == a.state.modal {
		a.resolveHostRequest(request, "", err)
		return true
	}
	for i, queued := range a.state.modalQueue {
		if queued == request {
			a.state.modalQueue = append(a.state.modalQueue[:i], a.state.modalQueue[i+1:]...)
			select {
			case request.reply <- hostResponse{err: err}:
			default:
			}
			return false
		}
	}
	return false
}

func (a *App) cancelHostRequests(err error) {
	// resolveHostRequest starts the next queued request, so repeat until the
	// entire rendezvous queue has been released.
	for a.state.modal != nil {
		a.resolveHostRequest(a.state.modal, "", err)
	}
	for _, request := range a.state.modalQueue {
		select {
		case request.reply <- hostResponse{err: err}:
		default:
		}
	}
	a.state.modalQueue = nil
}

func (a *App) activeEditor() *Editor {
	if a.state.modal != nil {
		return a.state.modal.editor
	}
	return a.state.editor
}
