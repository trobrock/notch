package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/config"
	"github.com/trobrock/notch/internal/credentials"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/modelregistry"
	"github.com/trobrock/notch/internal/session"
)

func TestSelectRunMode(t *testing.T) {
	tests := []struct {
		name                    string
		opts                    options
		interactive             bool
		wantFullscreen, wantOne bool
	}{
		{"interactive prompt opens TUI", options{prompt: "hello"}, true, true, false},
		{"print prompt exits", options{prompt: "hello", printMode: true}, true, false, true},
		{"redirected prompt exits", options{prompt: "hello"}, false, false, true},
		{"json prompt exits", options{prompt: "hello", jsonOutput: true}, true, false, true},
		{"no TUI prompt exits", options{prompt: "hello", noTUI: true}, true, false, true},
		{"empty interactive opens TUI", options{}, true, true, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fullscreen, oneShot := selectRunMode(test.opts, test.interactive)
			if fullscreen != test.wantFullscreen || oneShot != test.wantOne {
				t.Fatalf("selectRunMode() = (%v, %v), want (%v, %v)", fullscreen, oneShot, test.wantFullscreen, test.wantOne)
			}
		})
	}
}

func TestRootPlanFlagRemoved(t *testing.T) {
	var opts options
	flags := newRootFlagSet(&opts)
	if err := flags.Parse([]string{"--plan"}); err == nil {
		t.Fatal("removed --plan flag was accepted")
	}
}

func TestRootPrintAndProviderFlagsMatchPi(t *testing.T) {
	var opts options
	flags := newRootFlagSet(&opts)
	if err := flags.Parse([]string{"-p", "hello world"}); err != nil {
		t.Fatal(err)
	}
	if !opts.printMode || opts.provider != "" || flags.Arg(0) != "hello world" {
		t.Fatalf("parsed options = %+v, args = %q", opts, flags.Args())
	}

	opts = options{}
	flags = newRootFlagSet(&opts)
	if err := flags.Parse([]string{"--provider", "openai", "hello"}); err != nil {
		t.Fatal(err)
	}
	if opts.provider != "openai" || opts.printMode || flags.Arg(0) != "hello" {
		t.Fatalf("parsed options = %+v, args = %q", opts, flags.Args())
	}
}

func TestRootHelpListsCommands(t *testing.T) {
	output, err := captureStdout(t, func() error { return run([]string{"--help"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"version", "upgrade", "models", "login", "logout", "auth", "mcp", "extensions"} {
		if !strings.Contains(output, "  "+command) {
			t.Errorf("help does not list %q:\n%s", command, output)
		}
	}
	if !strings.Contains(output, "Options:") || !strings.Contains(output, "-provider") {
		t.Fatalf("help does not list options:\n%s", output)
	}
}

func TestRootHelpAliases(t *testing.T) {
	for _, argument := range []string{"help", "-h"} {
		t.Run(argument, func(t *testing.T) {
			output, err := captureStdout(t, func() error { return run([]string{argument}) })
			if err != nil || !strings.Contains(output, "Commands:") {
				t.Fatalf("output = %q, err = %v", output, err)
			}
		})
	}
}

func TestExtensionsSyncUsesDefaultConfigManifest(t *testing.T) {
	configHome, dataHome, source := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)
	manifestDir := filepath.Join(configHome, "notch")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"version":1,"packages":[{"name":"demo","source":"` + filepath.ToSlash(source) + `"}]}`
	if err := os.WriteFile(filepath.Join(manifestDir, "extensions.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := captureStdout(t, func() error { return runExtensions([]string{"sync", "--dry-run"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "Would install demo.") || !strings.Contains(output, "1 package(s) missing.") {
		t.Fatalf("output = %q", output)
	}
	if _, err := os.Stat(filepath.Join(dataHome, "notch", "packages.json")); !os.IsNotExist(err) {
		t.Fatalf("dry run created package state: %v", err)
	}
}

func TestAuthenticationCommandsIgnoreProjectConfig(t *testing.T) {
	cwd := t.TempDir()
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "notch-home"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "notch-home"))
	if err := os.MkdirAll(filepath.Join(cwd, ".notch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".notch", "config.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runAuth([]string{"auth", "status"}); err != nil {
		t.Fatalf("authentication honored project config: %v", err)
	}
}

func TestInitIgnoresProjectInputsAndAppliesModelFlags(t *testing.T) {
	t.Setenv("NOTCH_PROFILE", "")
	cwd := t.TempDir()
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })

	notchHome := filepath.Join(t.TempDir(), "notch-home")
	t.Setenv("XDG_CONFIG_HOME", notchHome)
	t.Setenv("XDG_DATA_HOME", notchHome)
	t.Setenv("NOTCH_EXPLORE_MODEL", "openai/explore-model")
	if err := os.WriteFile(filepath.Join(cwd, ".git"), []byte("malformed worktree marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, ".notch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".notch", "config.json"), []byte("{malformed project config"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := captureStdout(t, func() error {
		return run([]string{"--init", "--provider", "openai", "--model", "init-model", "--thinking", "high"})
	})
	if err != nil {
		t.Fatalf("init inspected project inputs: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(notchHome, "notch", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var initialized struct {
		Provider     string `json:"provider"`
		Model        string `json:"model"`
		ExploreModel string `json:"explore_model"`
		Thinking     string `json:"thinking_level"`
	}
	if err := json.Unmarshal(data, &initialized); err != nil {
		t.Fatal(err)
	}
	if initialized.Provider != "openai" || initialized.Model != "init-model" || initialized.ExploreModel != "openai/explore-model" || initialized.Thinking != "high" {
		t.Fatalf("initialized config = %+v", initialized)
	}
	if !strings.Contains(output, filepath.Join(cwd, ".notch", "extensions")) {
		t.Fatalf("init output did not retain workspace path: %q", output)
	}
	if _, err := os.Stat(filepath.Join(notchHome, "notch", "trusted-workspaces.json")); !os.IsNotExist(err) {
		t.Fatalf("init created workspace trust state: %v", err)
	}
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = old }()

	runErr := fn()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return string(data), runErr
}

func TestResolveWorkspaceTrustDoesNotCreateTrustStateWithoutInputs(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	notchHome := filepath.Join(t.TempDir(), "absent-notch-home")
	t.Setenv("XDG_CONFIG_HOME", notchHome)
	t.Setenv("XDG_DATA_HOME", notchHome)
	trusted, err := resolveWorkspaceTrust(home, root, root, options{}, strings.NewReader("yes\n"), &bytes.Buffer{}, true)
	if err != nil || trusted {
		t.Fatalf("trusted=%v err=%v", trusted, err)
	}
	if _, err := os.Stat(notchHome); !os.IsNotExist(err) {
		t.Fatalf("trust state created without project inputs: %v", err)
	}
}

func TestResolveWorkspaceTrustSafeAndNonInteractive(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "notch-home"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "notch-home"))
	if err := os.MkdirAll(filepath.Join(root, ".notch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".notch", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	trusted, err := resolveWorkspaceTrust(home, root, root, options{safe: true}, strings.NewReader("yes\n"), &output, true)
	if err != nil || trusted || output.Len() != 0 {
		t.Fatalf("safe: trusted=%v output=%q err=%v", trusted, output.String(), err)
	}
	trusted, err = resolveWorkspaceTrust(home, root, root, options{}, strings.NewReader("yes\n"), &output, false)
	if err != nil || trusted || output.Len() != 0 {
		t.Fatalf("noninteractive: trusted=%v output=%q err=%v", trusted, output.String(), err)
	}
}

func TestResolveWorkspaceTrustPromptsOnceAndPersists(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	notchHome := filepath.Join(t.TempDir(), "notch-home")
	t.Setenv("XDG_CONFIG_HOME", notchHome)
	t.Setenv("XDG_DATA_HOME", notchHome)
	if err := os.MkdirAll(filepath.Join(root, ".agents", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".agents", "skills", "test.md"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	trusted, err := resolveWorkspaceTrust(home, root, root, options{}, strings.NewReader("yes\n"), &output, true)
	if err != nil || !trusted || !strings.Contains(output.String(), "Trust workspace") {
		t.Fatalf("first resolution: trusted=%v output=%q err=%v", trusted, output.String(), err)
	}
	output.Reset()
	trusted, err = resolveWorkspaceTrust(home, root, root, options{}, strings.NewReader("no\n"), &output, true)
	if err != nil || !trusted || output.Len() != 0 {
		t.Fatalf("persisted resolution: trusted=%v output=%q err=%v", trusted, output.String(), err)
	}
}

func TestResolveWorkspaceTrustExplicitFlagPersistsWithoutInputs(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	notchHome := filepath.Join(t.TempDir(), "notch-home")
	t.Setenv("XDG_CONFIG_HOME", notchHome)
	t.Setenv("XDG_DATA_HOME", notchHome)
	trusted, err := resolveWorkspaceTrust(home, root, root, options{trustWorkspace: true}, strings.NewReader(""), &bytes.Buffer{}, false)
	if err != nil || !trusted {
		t.Fatalf("explicit trust: trusted=%v err=%v", trusted, err)
	}
	info, err := os.Stat(filepath.Join(notchHome, "notch", "trusted-workspaces.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("trust file mode = %04o", info.Mode().Perm())
	}
}

func TestResolveWorkspaceTrustWritesPromptToDiagnosticWriter(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "notch-home"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "notch-home"))
	if err := os.MkdirAll(filepath.Join(root, ".notch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".notch", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var diagnostic bytes.Buffer
	trusted, err := resolveWorkspaceTrust(home, root, root, options{jsonOutput: true}, strings.NewReader("no\n"), &diagnostic, true)
	if err != nil || trusted {
		t.Fatalf("trusted=%v err=%v", trusted, err)
	}
	if !strings.Contains(diagnostic.String(), "Trust workspace") {
		t.Fatalf("diagnostic prompt = %q", diagnostic.String())
	}
}

func TestResolveWorkspaceTrustExplicitFlagRepairsUnsafeMode(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	notchHome := filepath.Join(t.TempDir(), "notch-home")
	t.Setenv("XDG_CONFIG_HOME", notchHome)
	t.Setenv("XDG_DATA_HOME", notchHome)
	if err := os.MkdirAll(filepath.Join(notchHome, "notch"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(notchHome, "notch", "trusted-workspaces.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"workspaces":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	trusted, err := resolveWorkspaceTrust(home, root, root, options{trustWorkspace: true}, strings.NewReader(""), &bytes.Buffer{}, false)
	if err != nil || !trusted {
		t.Fatalf("explicit trust: trusted=%v err=%v", trusted, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("trust file mode = %04o", info.Mode().Perm())
	}
}

func TestToolPolicy(t *testing.T) {
	registry := extension.NewRegistry()
	for _, name := range []string{"read", "write", "bash"} {
		if err := registry.RegisterTool(extension.Tool{
			Definition: model.ToolDefinition{Name: name},
			Execute: func(context.Context, json.RawMessage, func(string)) (extension.ToolResult, error) {
				return extension.ToolResult{}, nil
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyToolPolicy(registry, options{toolAllow: "read, bash", toolExclude: "bash"}); err != nil {
		t.Fatal(err)
	}
	registered := registry.Tools()
	if len(registered) != 1 || registered[0].Definition.Name != "read" {
		t.Fatalf("tools = %#v", registered)
	}
	if err := applyToolPolicy(registry, options{toolAllow: "missing"}); err == nil {
		t.Fatal("missing allowlisted tool succeeded")
	}

	names, err := parseToolNames(" bash,read,bash ")
	if err != nil || !reflect.DeepEqual(names, []string{"bash", "read"}) {
		t.Fatalf("names = %#v, %v", names, err)
	}
	if _, err := parseToolNames("read,,bash"); err == nil {
		t.Fatal("empty tool name succeeded")
	}
}

func TestSessionLifecycleUsesFreshShutdownContext(t *testing.T) {
	registry := extension.NewRegistry()
	var started, stopped bool
	registry.On("session_start", "test", func(_ context.Context, event map[string]any) (map[string]any, error) {
		started = event["session_id"] == "session-1"
		return nil, nil
	})
	registry.On("session_shutdown", "test", func(ctx context.Context, event map[string]any) (map[string]any, error) {
		if err := ctx.Err(); err != nil {
			t.Fatalf("shutdown context was already canceled: %v", err)
		}
		stopped = event["session_id"] == "session-1" && event["reason"] == "canceled"
		return nil, nil
	})
	parent, cancel := context.WithCancel(context.Background())
	shutdown, err := beginSessionLifecycle(parent, registry, map[string]any{"session_id": "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := shutdown("canceled"); err != nil {
		t.Fatal(err)
	}
	if !started || !stopped {
		t.Fatalf("started = %v, stopped = %v", started, stopped)
	}
}

func TestWriteModelListJSON(t *testing.T) {
	models := []modelregistry.Entry{{Provider: "anthropic", ID: "claude-test", Name: "Claude Test", ContextWindow: 200000, Reasoning: true, Source: "builtin"}}
	var output bytes.Buffer
	if err := writeModelList(&output, models, true); err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Version int                   `json:"version"`
		Models  []modelregistry.Entry `json:"models"`
	}
	if err := json.Unmarshal(output.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Version != 1 || !reflect.DeepEqual(catalog.Models, models) {
		t.Fatalf("catalog = %#v", catalog)
	}

	output.Reset()
	if err := writeModelList(&output, models, false); err != nil {
		t.Fatal(err)
	}
	if text := output.String(); !strings.Contains(text, "anthropic") || !strings.Contains(text, "claude-test") {
		t.Fatalf("text model list = %q", text)
	}
}

func TestValidThinkingLevel(t *testing.T) {
	for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh"} {
		if !validThinkingLevel(level) {
			t.Errorf("%q rejected", level)
		}
	}
	for _, level := range []string{"", "max", "HIGH"} {
		if validThinkingLevel(level) {
			t.Errorf("%q accepted", level)
		}
	}
}

func TestAnthropicProviderAuthenticationIsSeparated(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "legacy-oauth-token")
	store := credentials.New(filepath.Join(t.TempDir(), "auth.json"))
	if err := store.Put(credentials.LegacyAnthropicProvider, credentials.Credential{Type: "oauth", Access: "stored-oauth-token"}); err != nil {
		t.Fatal(err)
	}

	if _, err := makeProvider(context.Background(), config.Config{Provider: "anthropic"}, store); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("direct Anthropic provider accepted OAuth credentials: %v", err)
	}
	if _, err := makeProvider(context.Background(), config.Config{Provider: "anthropic-claude-code"}, store); err != nil {
		t.Fatalf("Claude Code subscription provider rejected OAuth credential: %v", err)
	}
}

func TestLogoutAnthropicClaudeCodeRemovesLegacyCredential(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	dataHome := filepath.Join(t.TempDir(), "data")
	t.Setenv("XDG_DATA_HOME", dataHome)
	store := credentials.New(filepath.Join(dataHome, "notch", "auth.json"))
	credential := credentials.Credential{Type: "oauth", Access: "legacy-token"}
	if err := store.Put(credentials.LegacyAnthropicProvider, credential); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.GetWithLegacyFallback(credentials.AnthropicClaudeCodeProvider, credentials.LegacyAnthropicProvider); err != nil {
		t.Fatal(err)
	}
	if err := runAuth([]string{"logout", "anthropic-claude-code"}); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{credentials.AnthropicClaudeCodeProvider, credentials.LegacyAnthropicProvider} {
		if _, ok, err := store.Get(provider); err != nil || ok {
			t.Fatalf("credential %q remains after logout: ok=%v err=%v", provider, ok, err)
		}
	}
}

func TestNormalizeProviderClaudeSelectsSubscription(t *testing.T) {
	if got := normalizeProvider("Claude"); got != "anthropic-claude-code" {
		t.Fatalf("normalizeProvider(Claude) = %q", got)
	}
	if got := normalizeProvider("anthropic"); got != "anthropic" {
		t.Fatalf("normalizeProvider(anthropic) = %q", got)
	}
}

func TestRPCAPIForProvider(t *testing.T) {
	want := map[string]string{"anthropic": "anthropic-messages", "anthropic-claude-code": "anthropic-messages", "openrouter": "openai-completions", "openai-codex": "openai-codex-responses", "openai": "openai-responses"}
	for provider, api := range want {
		if got := rpcAPIForProvider(provider); got != api {
			t.Errorf("%s API = %q", provider, got)
		}
	}
}

func TestCurrentBuildInfoUsesInjectedValues(t *testing.T) {
	oldVersion, oldCommit, oldBuildDate := version, commit, buildDate
	version, commit, buildDate = "v1.2.3", "abc123", "2026-01-02T03:04:05Z"
	t.Cleanup(func() { version, commit, buildDate = oldVersion, oldCommit, oldBuildDate })

	info := currentBuildInfo()
	if info.Version != "v1.2.3" || info.Commit != "abc123" || info.BuildDate != "2026-01-02T03:04:05Z" {
		t.Fatalf("build info = %#v", info)
	}
	if info.GoVersion == "" || info.Platform == "" {
		t.Fatalf("incomplete build info = %#v", info)
	}
}

func TestBenchmarkFlags(t *testing.T) {
	var opts options
	flags := newRootFlagSet(&opts)
	if err := flags.Parse([]string{"--max-turns", "7", "--max-cost-usd", "2.5", "--idle-timeout", "30s", "--setting-sources", "project"}); err != nil {
		t.Fatal(err)
	}
	if opts.maxTurns != 7 || opts.maxCostUSD != 2.5 || opts.idleTimeout != 30*time.Second || opts.settingSources != "project" {
		t.Fatalf("options = %+v", opts)
	}
	user, project, err := parseSettingSources(opts.settingSources)
	if err != nil || user || !project {
		t.Fatalf("sources = %v, %v, %v", user, project, err)
	}
	if processExitCode(agent.ErrMaxTurns) != 2 || processExitCode(agent.ErrMaxCost) != 2 || processExitCode(errors.New("bad key")) != 1 {
		t.Fatal("unexpected exit code classification")
	}
}

func TestWorkspaceInstructionsRespectProjectSettingSource(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("project-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                    string
		trusted, includeProject bool
		wantProject             bool
	}{
		{"trusted project source", true, true, true},
		{"trusted user source", true, false, false},
		{"untrusted project source", false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := loadWorkspaceInstructions(root, test.trusted, test.includeProject)
			if err != nil || strings.Contains(got, "project-only") != test.wantProject {
				t.Fatalf("instructions = %q, %v", got, err)
			}
		})
	}
}

func TestRootResumeFlagAllowsPickerOrExplicitSession(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--resume"}, ""},
		{[]string{"--resume", "session-id"}, "session-id"},
		{[]string{"--resume=session-id"}, "session-id"},
	} {
		var opts options
		flags := newRootFlagSet(&opts)
		if err := flags.Parse(normalizeResumeArgs(tc.args)); err != nil {
			t.Fatal(err)
		}
		if !opts.resumeSpecified || opts.resumeSession != tc.want {
			t.Fatalf("args %v parsed as %+v", tc.args, opts)
		}
	}
}

func TestRunModeClassifiesPrintFromArgumentOrStdin(t *testing.T) {
	if got := runMode(false, false, options{printMode: true}); got != "print" {
		t.Fatalf("stdin print mode = %q", got)
	}
	if got := runMode(false, false, options{printMode: true, prompt: "hello"}); got != "print" {
		t.Fatalf("argument print mode = %q", got)
	}
}

func TestRunResumeUsesCurrentSettings(t *testing.T) {
	for _, mode := range []string{"--continue", "--resume"} {
		for _, tc := range []struct {
			name, storedProfile, activeProfile string
		}{
			{"legacy", "", "work"},
			{"cross-profile", "personal", "work"},
			{"changed-profile", "work", "work"},
			{"without-profile", "personal", ""},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				home, configHome, dataHome := t.TempDir(), t.TempDir(), t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("XDG_CONFIG_HOME", configHome)
				t.Setenv("XDG_DATA_HOME", dataHome)
				t.Setenv("NOTCH_PROFILE", tc.activeProfile)
				t.Setenv("NOTCH_PROVIDER", "")
				t.Setenv("NOTCH_MODEL", "")
				t.Setenv("NOTCH_EXPLORE_MODEL", "")
				t.Setenv("NOTCH_THINKING_LEVEL", "off")
				t.Setenv("ANTHROPIC_API_KEY", "test-key")
				requests := make(chan map[string]any, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, `{"data":[{"id":"claude-sonnet-4-6","type":"model","display_name":"Current model"}],"has_more":false}`)
						return
					}
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					requests <- request
					w.Header().Set("Content-Type", "text/event-stream")
					for _, event := range []string{
						`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
						`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"resumed"}}`,
						`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
						`{"type":"message_stop"}`,
					} {
						io.WriteString(w, "data: "+event+"\n\n")
					}
				}))
				defer server.Close()
				configDir := filepath.Join(configHome, "notch")
				if err := os.MkdirAll(configDir, 0o755); err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(map[string]any{
					"provider": "anthropic", "model": "claude-sonnet-4-6", "base_url": server.URL,
					"disable_pricing_refresh": true,
					"profiles": map[string]any{"work": map[string]any{
						"provider": "anthropic", "model": "claude-sonnet-4-6", "allowed_providers": []string{"anthropic"},
					}},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(configDir, "config.json"), body, 0o600); err != nil {
					t.Fatal(err)
				}
				cfg, err := config.LoadGlobal(home)
				if err != nil {
					t.Fatal(err)
				}
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				var routing *config.RoutingSnapshot
				if tc.storedProfile != "" {
					routing = &config.RoutingSnapshot{Profile: tc.storedProfile, Provider: "openai", Model: "old-model", DefinitionHash: "old-definition"}
				}
				store, err := session.NewWithRouting(cfg.SessionDir, cwd, "openai", "old-model", routing)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.AppendMessage(model.TextMessage("user", "saved context")); err != nil {
					t.Fatal(err)
				}
				if err := store.AppendMessage(model.TextMessage("assistant", "saved answer")); err != nil {
					t.Fatal(err)
				}
				path := store.Path()
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				args := []string{"--setting-sources=user", "--no-tui", "--print", "--no-tools", "--no-extensions", "--no-resources", mode}
				if mode == "--resume" {
					args = append(args, path)
				}
				args = append(args, "continue now")
				if err := run(args); err != nil {
					t.Fatal(err)
				}
				select {
				case request := <-requests:
					if request["model"] != "claude-sonnet-4-6" {
						t.Errorf("requested model = %v", request["model"])
					}
					messages, _ := json.Marshal(request["messages"])
					if !bytes.Contains(messages, []byte("saved context")) || !bytes.Contains(messages, []byte("continue now")) {
						t.Errorf("request lost session context: %s", messages)
					}
				default:
					t.Fatal("no request reached the current provider")
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.HasPrefix(after, before) || len(after) <= len(before) {
					t.Fatal("resume did not append to the original session")
				}
				loaded, err := session.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				defer loaded.Close()
				if len(loaded.Messages) != 4 || loaded.Header.Profile != tc.storedProfile || loaded.Header.Provider != "openai" {
					t.Fatalf("session history or original metadata changed: %+v", loaded.Header)
				}
			})
		}
	}
}

func TestConfigForProviderClearsProviderSpecificCredentialReference(t *testing.T) {
	base := config.Config{Provider: "anthropic-claude-code", Model: "old", BaseURL: "https://old.example", CredentialRef: "work-login"}
	same := configForProvider(base, "anthropic-claude-code")
	if same.CredentialRef != "work-login" {
		t.Fatalf("same-provider credential ref = %q", same.CredentialRef)
	}
	changed := configForProvider(base, "openai-codex")
	if changed.CredentialRef != "" || changed.BaseURL != "" {
		t.Fatalf("changed config = %+v", changed)
	}
}

func TestOpenRouterCredentialReferenceOverridesAmbientEnvironment(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		http.Error(w, "stop", http.StatusUnauthorized)
	}))
	defer server.Close()
	t.Setenv("OPENROUTER_API_KEY", "ambient-key")
	store := credentials.New(filepath.Join(t.TempDir(), "auth.json"))
	if err := store.Put("openrouter:work", credentials.Credential{Type: "api_key", Access: "referenced-key"}); err != nil {
		t.Fatal(err)
	}
	provider, err := makeProvider(context.Background(), config.Config{Provider: "openrouter", BaseURL: server.URL, CredentialRef: "openrouter:work"}, store)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = provider.Stream(context.Background(), model.Request{Model: "test", Messages: []model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "hi"}}}}}, func(model.StreamEvent) {})
	if authorization != "Bearer referenced-key" {
		t.Fatalf("Authorization = %q", authorization)
	}
}

func TestSelectedProfileNameUsesEnvironmentAndCLIOverridesIt(t *testing.T) {
	t.Setenv("NOTCH_PROFILE", "personal")
	if got := selectedProfileName(""); got != "personal" {
		t.Fatalf("environment profile = %q", got)
	}
	if got := selectedProfileName(" work "); got != "work" {
		t.Fatalf("CLI profile = %q", got)
	}
}

func TestRunRejectsUnknownEnvironmentProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("NOTCH_PROFILE", "missing")
	err := run([]string{"--setting-sources=none", "--show-effective-settings"})
	if err == nil || !strings.Contains(err.Error(), `unknown profile "missing"`) {
		t.Fatalf("error = %v", err)
	}
}
