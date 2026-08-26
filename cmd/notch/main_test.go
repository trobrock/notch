package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/config"
	"github.com/trobrock/notch/internal/credentials"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/modelregistry"
)

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
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Thinking string `json:"thinking_level"`
	}
	if err := json.Unmarshal(data, &initialized); err != nil {
		t.Fatal(err)
	}
	if initialized.Provider != "openai" || initialized.Model != "init-model" || initialized.Thinking != "high" {
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
