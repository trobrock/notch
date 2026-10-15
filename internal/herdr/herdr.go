// Package herdr reports Notch lifecycle state when it runs inside a Herdr pane.
package herdr

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/trobrock/notch/internal/extension"
)

const (
	source         = "notch"
	commandTimeout = time.Second
)

type executor func(context.Context, string, ...string) error

// Session describes the Notch session Herdr should restore in this pane.
type Session struct {
	ID       string
	Provider string
	Model    string
	Thinking string
	Mode     string
}

type report struct {
	state   string
	message string
	session Session
}

// Reporter sends best-effort, ordered state updates to one Herdr pane.
type Reporter struct {
	binary string
	paneID string
	exec   executor

	ctx    context.Context
	cancel context.CancelFunc
	queue  chan report
	wg     sync.WaitGroup
	seq    atomic.Int64

	mu           sync.Mutex
	closed       bool
	state        string
	message      string
	session      Session
	blocks       int
	blockMessage string
}

// NewFromEnvironment returns nil outside a Herdr-managed pane.
func NewFromEnvironment() *Reporter {
	if os.Getenv("HERDR_ENV") != "1" {
		return nil
	}
	binary := strings.TrimSpace(os.Getenv("HERDR_BIN_PATH"))
	paneID := strings.TrimSpace(os.Getenv("HERDR_PANE_ID"))
	if binary == "" || paneID == "" {
		return nil
	}
	return newReporter(binary, paneID, runCommand)
}

func newReporter(binary, paneID string, run executor) *Reporter {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Reporter{
		binary: binary,
		paneID: paneID,
		exec:   run,
		ctx:    ctx,
		cancel: cancel,
		queue:  make(chan report, 1),
		state:  "idle",
	}
	r.seq.Store(time.Now().UnixNano())
	r.wg.Add(1)
	go r.work()
	return r
}

func runCommand(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

// Register connects agent and session lifecycle hooks to the reporter.
func (r *Reporter) Register(registry *extension.Registry) {
	if r == nil || registry == nil {
		return
	}
	registry.On("before_agent_start", "builtin:herdr", func(_ context.Context, event map[string]any) (map[string]any, error) {
		r.beginRun(event)
		return event, nil
	})
	registry.On("run_end", "builtin:herdr", func(_ context.Context, event map[string]any) (map[string]any, error) {
		r.SetState("idle", "")
		return event, nil
	})
	registry.On("session_change", "builtin:herdr", func(_ context.Context, event map[string]any) (map[string]any, error) {
		id, _ := event["session_id"].(string)
		r.SetSessionID(id)
		return event, nil
	})
}

// Start claims the pane as an idle Notch agent and publishes its resume command.
func (r *Reporter) Start(session Session) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.session = session
	r.state, r.message = "idle", ""
	r.enqueueLocked()
	r.mu.Unlock()
}

// SetSessionID publishes a changed session while preserving resolved launch settings.
func (r *Reporter) SetSessionID(id string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.session.ID = id
	r.enqueueLocked()
	r.mu.Unlock()
}

// SetModel refreshes the model flags in the reported resume command.
func (r *Reporter) SetModel(provider, model string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.session.Provider, r.session.Model = provider, model
	r.enqueueLocked()
	r.mu.Unlock()
}

// SetThinking refreshes the thinking flag in the reported resume command.
func (r *Reporter) SetThinking(level string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.session.Thinking = level
	r.enqueueLocked()
	r.mu.Unlock()
}

// SetState changes the semantic state Herdr uses for waits and notifications.
func (r *Reporter) SetState(state, message string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.state, r.message = state, message
	if r.blocks == 0 {
		r.enqueueLocked()
	}
	r.mu.Unlock()
}

func (r *Reporter) beginRun(event map[string]any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if value, ok := event["provider"].(string); ok && value != "" {
		r.session.Provider = value
	}
	if value, ok := event["model"].(string); ok && value != "" {
		r.session.Model = value
	}
	if value, ok := event["thinking_level"].(string); ok && value != "" {
		r.session.Thinking = value
	}
	r.state, r.message = "working", ""
	if r.blocks == 0 {
		r.enqueueLocked()
	}
	r.mu.Unlock()
}

func (r *Reporter) beginBlock(message string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.blocks++
	if r.blocks == 1 {
		r.blockMessage = message
		r.enqueueLocked()
	}
	r.mu.Unlock()
}

func (r *Reporter) endBlock() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.blocks > 0 {
		r.blocks--
	}
	if r.blocks == 0 {
		r.blockMessage = ""
		r.enqueueLocked()
	}
	r.mu.Unlock()
}

func (r *Reporter) enqueueLocked() {
	if r.closed {
		return
	}
	next := report{state: r.state, message: r.message, session: r.session}
	if r.blocks > 0 {
		next.state, next.message = "blocked", r.blockMessage
	}
	r.enqueue(next)
}

func (r *Reporter) enqueue(next report) {
	select {
	case r.queue <- next:
		return
	default:
	}
	select {
	case <-r.queue:
	default:
	}
	select {
	case r.queue <- next:
	default:
	}
}

func (r *Reporter) work() {
	defer r.wg.Done()
	for {
		select {
		case <-r.ctx.Done():
			return
		case next := <-r.queue:
			r.sendState(r.ctx, next)
		}
	}
}

func (r *Reporter) sendState(parent context.Context, next report) {
	args := []string{"pane", "report-agent", r.paneID,
		"--source", source, "--agent", source,
		"--state", next.state, "--seq", strconv.FormatInt(r.seq.Add(1), 10)}
	if next.message != "" {
		args = append(args, "--message", next.message)
	}
	if resume := resumeArgs(next.session); len(resume) != 0 {
		args = append(args, "--agent-session-id", next.session.ID, "--")
		args = append(args, resume...)
	}
	ctx, cancel := context.WithTimeout(parent, commandTimeout)
	defer cancel()
	_ = r.exec(ctx, r.binary, args...)
}

// Close stops pending reports and releases the pane. It is safe to call more than once.
func (r *Reporter) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()

	r.cancel()
	r.wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	_ = r.exec(ctx, r.binary, "pane", "release-agent", r.paneID,
		"--source", source, "--agent", source, "--seq", strconv.FormatInt(r.seq.Add(1), 10))
}

func resumeArgs(session Session) []string {
	if session.ID == "" || session.Mode == "print" {
		return nil
	}
	args := []string{"notch", "--resume", session.ID}
	if session.Provider != "" {
		args = append(args, "--provider", session.Provider)
	}
	if session.Model != "" {
		args = append(args, "--model", session.Model)
	}
	if session.Thinking != "" {
		args = append(args, "--thinking", session.Thinking)
	}
	switch session.Mode {
	case "rpc":
		args = append(args, "--rpc")
	case "line":
		args = append(args, "--no-tui")
	case "json":
		args = append(args, "--json")
	}
	if !validResumeArgs(args) {
		return nil
	}
	return args
}

func validResumeArgs(args []string) bool {
	if len(args) == 0 || len(args) > 64 {
		return false
	}
	total := 0
	for i, arg := range args {
		total += len(arg)
		if i != 0 {
			total++
		}
		if strings.ContainsRune(arg, '\'') {
			return false
		}
		for _, value := range arg {
			if unicode.IsControl(value) {
				return false
			}
		}
	}
	return total <= 8*1024
}

// WrapHost reports blocked while an extension waits for user input.
func WrapHost(host extension.Host, reporter *Reporter) extension.Host {
	if host == nil || reporter == nil {
		return host
	}
	return &reportingHost{Host: host, reporter: reporter}
}

type reportingHost struct {
	extension.Host
	reporter *Reporter
}

func (h *reportingHost) Input(ctx context.Context, prompt, placeholder string) (string, error) {
	h.reporter.beginBlock(prompt)
	defer h.reporter.endBlock()
	return h.Host.Input(ctx, prompt, placeholder)
}

func (h *reportingHost) Select(ctx context.Context, prompt string, options []string) (string, error) {
	h.reporter.beginBlock(prompt)
	defer h.reporter.endBlock()
	return h.Host.Select(ctx, prompt, options)
}

func (h *reportingHost) FollowUpFor(sessionID, message string) error {
	if guarded, ok := h.Host.(interface{ FollowUpFor(string, string) error }); ok {
		return guarded.FollowUpFor(sessionID, message)
	}
	if h.Host.SessionID() != sessionID {
		return errors.New("active session changed before extension follow-up was delivered")
	}
	return h.Host.FollowUp(message)
}
