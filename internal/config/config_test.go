package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultSystemPromptGuidesCodebaseExploration(t *testing.T) {
	cfg := Defaults(t.TempDir(), t.TempDir())
	for _, text := range []string{"use explore_codebase proactively", "direct grep", "exactly one of task", "never provide both"} {
		if !strings.Contains(cfg.SystemPrompt, text) {
			t.Fatalf("default system prompt missing %q: %q", text, cfg.SystemPrompt)
		}
	}
}

func TestDefaultsAndNotchHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	cwd := filepath.Join(t.TempDir(), "project")
	t.Setenv("NOTCH_HOME", "")

	cfg := Defaults(home, cwd)
	root := filepath.Join(home, ".notch")
	if cfg.Provider == "" || cfg.Model == "" || cfg.MaxTokens == 0 || cfg.SystemPrompt == "" {
		t.Fatalf("incomplete defaults: %+v", cfg)
	}
	if cfg.Theme != "dark" || cfg.ThinkingLevel != "medium" || cfg.MouseCapture == nil || !*cfg.MouseCapture || cfg.Compaction == nil || cfg.Compaction.Enabled == nil || !*cfg.Compaction.Enabled {
		t.Fatalf("interactive defaults are incomplete: %+v", cfg)
	}
	if cfg.MCPConfig != filepath.Join(root, "mcp.json") || cfg.MCPAuthFile != filepath.Join(root, "mcp-auth.json") || cfg.SessionDir != filepath.Join(root, "sessions") || cfg.ModelCache != filepath.Join(root, "models.json") || cfg.ModelRefreshHours != 24 {
		t.Fatalf("defaults use wrong home: %+v", cfg)
	}
	wantExtensions := []string{filepath.Join(root, "extensions"), filepath.Join(cwd, ".notch", "extensions")}
	if !reflect.DeepEqual(cfg.ExtensionDirs, wantExtensions) {
		t.Fatalf("extension dirs = %#v, want %#v", cfg.ExtensionDirs, wantExtensions)
	}
	wantThemes := []string{filepath.Join(root, "themes"), filepath.Join(cwd, ".notch", "themes")}
	if !reflect.DeepEqual(cfg.ThemeDirs, wantThemes) {
		t.Fatalf("theme dirs = %#v, want %#v", cfg.ThemeDirs, wantThemes)
	}
	wantAgentSkills := []string{filepath.Join(home, ".agents", "skills"), filepath.Join(cwd, ".agents", "skills")}
	wantAgentCommands := []string{filepath.Join(home, ".agents", "commands"), filepath.Join(cwd, ".agents", "commands")}
	if !reflect.DeepEqual(cfg.AgentSkillDirs, wantAgentSkills) || !reflect.DeepEqual(cfg.AgentCommandDirs, wantAgentCommands) {
		t.Fatalf(".agents dirs = %#v / %#v", cfg.AgentSkillDirs, cfg.AgentCommandDirs)
	}
	wantSkillOrder := []string{wantAgentSkills[0], filepath.Join(root, "skills"), filepath.Join(cwd, ".notch", "skills"), wantAgentSkills[1]}
	if !reflect.DeepEqual(cfg.SkillDiscoveryDirs(), wantSkillOrder) {
		t.Fatalf("skill discovery order = %#v, want %#v", cfg.SkillDiscoveryDirs(), wantSkillOrder)
	}

	custom := filepath.Join(t.TempDir(), "custom-notch")
	t.Setenv("NOTCH_HOME", custom)
	cfg = Defaults(home, cwd)
	if cfg.SessionDir != filepath.Join(custom, "sessions") || cfg.MCPConfig != filepath.Join(custom, "mcp.json") || cfg.MCPAuthFile != filepath.Join(custom, "mcp-auth.json") {
		t.Fatalf("NOTCH_HOME not respected: %+v", cfg)
	}
}

func TestRelativeNotchHomeIsNormalizedAbsolute(t *testing.T) {
	cwd := t.TempDir()
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })
	t.Setenv("NOTCH_HOME", filepath.Join("state", "notch"))

	want := filepath.Join(cwd, "state", "notch")
	if got := HomeDir(t.TempDir()); got != want {
		t.Fatalf("HomeDir = %q, want %q", got, want)
	}
	cfg := Defaults(t.TempDir(), t.TempDir())
	if cfg.SessionDir != filepath.Join(want, "sessions") || cfg.MCPConfig != filepath.Join(want, "mcp.json") {
		t.Fatalf("relative NOTCH_HOME was not normalized consistently: %+v", cfg)
	}
}

func TestLoadMergesUserThenProject(t *testing.T) {
	clearRuntimeConfigEnv(t)
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("NOTCH_HOME", "")
	writeJSON(t, filepath.Join(home, ".notch", "config.json"), `{
		"provider":"openai", "model":"global-model", "base_url":"https://global.test",
		"max_tokens":123, "system_prompt":"global prompt", "extension_dirs":["global-ext"],
		"skill_dirs":["global-skill"], "prompt_dirs":["global-prompt"], "theme_dirs":["global-theme"], "session_dir":"global-sessions",
		"theme":"dracula", "thinking_level":"low", "mouse":false, "context_window":99999, "model_cache":"custom-models.json", "model_refresh_hours":12,
		"compaction":{"enabled":false,"reserve_tokens":1000,"keep_recent_tokens":2000}
	}`)
	writeJSON(t, filepath.Join(cwd, ".notch", "config.json"), `{
		"model":"project-model", "max_tokens":456, "mcp_config":"project-mcp.json",
		"extension_dirs":["project-ext"], "provider":"", "prompt_dirs":[], "theme_dirs":["project-theme"],
		"thinking_level":"high", "compaction":{"keep_recent_tokens":3000}
	}`)

	cfg, err := Load(home, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "openai" || cfg.Model != "project-model" || cfg.BaseURL != "https://global.test" || cfg.MaxTokens != 456 {
		t.Fatalf("scalar merge failed: %+v", cfg)
	}
	if cfg.SystemPrompt != "global prompt" || cfg.MCPConfig != filepath.Join(cwd, "project-mcp.json") || cfg.SessionDir != "global-sessions" {
		t.Fatalf("scalar inheritance failed: %+v", cfg)
	}
	if cfg.Theme != "dracula" || cfg.ThinkingLevel != "high" || cfg.MouseCapture == nil || *cfg.MouseCapture || cfg.ContextWindow != 99999 || cfg.ModelCache != "custom-models.json" || cfg.ModelRefreshHours != 12 || cfg.Compaction == nil || cfg.Compaction.Enabled == nil || *cfg.Compaction.Enabled || cfg.Compaction.ReserveTokens != 1000 || cfg.Compaction.KeepRecentTokens != 3000 {
		t.Fatalf("theme/thinking/compaction merge failed: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.ExtensionDirs, []string{filepath.Join(cwd, "project-ext")}) ||
		!reflect.DeepEqual(cfg.SkillDirs, []string{"global-skill"}) ||
		!reflect.DeepEqual(cfg.PromptDirs, []string{"global-prompt"}) ||
		!reflect.DeepEqual(cfg.ThemeDirs, []string{filepath.Join(cwd, "project-theme")}) {
		t.Fatalf("directory merge failed: %+v", cfg)
	}
}

func TestLoadMissingAndMalformed(t *testing.T) {
	clearRuntimeConfigEnv(t)
	t.Setenv("NOTCH_HOME", "")
	if _, err := Load(t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("missing configs should be ignored: %v", err)
	}

	home := t.TempDir()
	writeJSON(t, filepath.Join(home, ".notch", "config.json"), `{bad`)
	_, err := Load(home, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), filepath.Join(home, ".notch", "config.json")) {
		t.Fatalf("expected path-bearing parse error, got %v", err)
	}
}

func TestLoadUsesNotchHomeForUserConfig(t *testing.T) {
	clearRuntimeConfigEnv(t)
	home, cwd, custom := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("NOTCH_HOME", custom)
	writeJSON(t, filepath.Join(home, ".notch", "config.json"), `{"model":"wrong"}`)
	writeJSON(t, filepath.Join(custom, "config.json"), `{"model":"right"}`)
	cfg, err := Load(home, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "right" {
		t.Fatalf("model = %q, want config below NOTCH_HOME", cfg.Model)
	}
}

func TestLoadEnvironmentOverridesFilesAndEmptyValuesFallBack(t *testing.T) {
	clearRuntimeConfigEnv(t)
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("NOTCH_HOME", "")
	writeJSON(t, filepath.Join(cwd, ".notch", "config.json"), `{"provider":"anthropic","model":"file-model","thinking_level":"low"}`)
	t.Setenv("NOTCH_PROVIDER", " openrouter ")
	t.Setenv("NOTCH_MODEL", " env-model ")
	t.Setenv("NOTCH_THINKING_LEVEL", " high ")

	cfg, err := Load(home, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "openrouter" || cfg.Model != "env-model" || cfg.ThinkingLevel != "high" {
		t.Fatalf("environment overrides = %+v", cfg)
	}

	t.Setenv("NOTCH_PROVIDER", "")
	t.Setenv("NOTCH_MODEL", "  ")
	t.Setenv("NOTCH_THINKING_LEVEL", "")
	cfg, err = Load(home, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "anthropic" || cfg.Model != "file-model" || cfg.ThinkingLevel != "low" {
		t.Fatalf("empty environment values did not fall back: %+v", cfg)
	}
}

func TestLoadWorkspaceUntrustedSkipsAllProjectInputs(t *testing.T) {
	clearRuntimeConfigEnv(t)
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("NOTCH_HOME", "")
	writeJSON(t, filepath.Join(home, ".notch", "config.json"), `{"model":"global","extension_dirs":["global-ext"]}`)
	writeJSON(t, filepath.Join(root, ".notch", "config.json"), `{"model":"project","extension_dirs":["project-ext"]}`)

	cfg, err := LoadWorkspace(home, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "global" || !reflect.DeepEqual(cfg.ExtensionDirs, []string{"global-ext"}) {
		t.Fatalf("project config loaded without trust: %+v", cfg)
	}
	paths := append([]string{}, cfg.SkillDirs...)
	paths = append(paths, cfg.PromptDirs...)
	paths = append(paths, cfg.ThemeDirs...)
	paths = append(paths, cfg.AgentSkillDirs...)
	paths = append(paths, cfg.AgentCommandDirs...)
	for _, path := range paths {
		if strings.HasPrefix(filepath.Clean(path), filepath.Clean(root)+string(filepath.Separator)) {
			t.Errorf("untrusted project discovery path loaded: %q", path)
		}
	}
}

func TestLoadWorkspaceUsesTrustedProjectMCP(t *testing.T) {
	clearRuntimeConfigEnv(t)
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("NOTCH_HOME", "")
	path := filepath.Join(root, ".notch", "mcp.json")
	writeJSON(t, path, `{}`)
	trusted, err := LoadWorkspace(home, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if trusted.MCPConfig != path {
		t.Fatalf("trusted MCP config = %q, want %q", trusted.MCPConfig, path)
	}
	untrusted, err := LoadWorkspace(home, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if untrusted.MCPConfig == path {
		t.Fatalf("untrusted project MCP loaded: %+v", untrusted)
	}
}

func TestLoadWorkspaceKeepsSensitiveFieldsGlobalOnly(t *testing.T) {
	clearRuntimeConfigEnv(t)
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("NOTCH_HOME", "")
	writeJSON(t, filepath.Join(home, ".notch", "config.json"), `{
		"base_url":"https://global.test", "auth_file":"global-auth", "mcp_auth_file":"global-mcp-auth", "session_dir":"global-sessions", "model_cache":"global-models"
	}`)
	writeJSON(t, filepath.Join(root, ".notch", "config.json"), `{
		"model":"trusted-project", "base_url":"https://evil.test", "auth_file":"evil-auth", "mcp_auth_file":"evil-mcp-auth", "session_dir":"evil-sessions", "model_cache":"evil-models"
	}`)

	cfg, err := LoadWorkspace(home, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "trusted-project" {
		t.Fatalf("ordinary trusted project field not loaded: %+v", cfg)
	}
	if cfg.BaseURL != "https://global.test" || cfg.AuthFile != "global-auth" || cfg.MCPAuthFile != "global-mcp-auth" || cfg.SessionDir != "global-sessions" || cfg.ModelCache != "global-models" {
		t.Fatalf("project overrode sensitive global fields: %+v", cfg)
	}
}

func TestLoadWorkspaceClearsGlobalBaseURLWhenProjectChangesProvider(t *testing.T) {
	clearRuntimeConfigEnv(t)
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("NOTCH_HOME", "")
	writeJSON(t, filepath.Join(home, ".notch", "config.json"), `{"provider":"openai","base_url":"https://global.test"}`)
	writeJSON(t, filepath.Join(root, ".notch", "config.json"), `{"provider":"anthropic"}`)

	cfg, err := LoadWorkspace(home, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "anthropic" || cfg.BaseURL != "" {
		t.Fatalf("project provider inherited global endpoint: %+v", cfg)
	}
}

func TestLoadGlobalUsesOnlyGlobalConfig(t *testing.T) {
	clearRuntimeConfigEnv(t)
	home := t.TempDir()
	t.Setenv("NOTCH_HOME", "")
	writeJSON(t, filepath.Join(home, ".notch", "config.json"), `{"auth_file":"global-auth"}`)
	cfg, err := LoadGlobal(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthFile != "global-auth" {
		t.Fatalf("global auth file = %q", cfg.AuthFile)
	}
	if len(cfg.ExtensionDirs) != 1 || len(cfg.AgentSkillDirs) != 1 || len(cfg.AgentCommandDirs) != 1 {
		t.Fatalf("global config contains project discovery dirs: %+v", cfg)
	}
}

func TestEnsureDirs(t *testing.T) {
	root := t.TempDir()
	dirs := []string{
		filepath.Join(root, "extensions", "nested"),
		filepath.Join(root, "skills"),
		filepath.Join(root, "prompts"),
		filepath.Join(root, "themes"),
		filepath.Join(root, "sessions"),
	}
	cfg := Config{
		ExtensionDirs: []string{dirs[0], dirs[0], ""},
		SkillDirs:     []string{dirs[1]},
		PromptDirs:    []string{dirs[2]},
		ThemeDirs:     []string{dirs[3]},
		SessionDir:    dirs[4],
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("directory %q was not created: %v", dir, err)
		}
	}
}

func TestEnsureGlobalDirsDoesNotCreateProjectOrArbitraryConfiguredPaths(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "notch-home")
	project := filepath.Join(root, "project", ".notch", "extensions")
	outside := filepath.Join(root, "outside")
	cfg := Config{
		ExtensionDirs: []string{filepath.Join(global, "extensions"), project, outside},
		AuthFile:      filepath.Join(global, "auth.json"),
		SessionDir:    filepath.Join(global, "sessions"),
		notchHome:     global,
	}
	if err := cfg.EnsureGlobalDirs(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{project, outside} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("created non-global discovery path %q: %v", path, err)
		}
	}
	for _, path := range []string{filepath.Join(global, "extensions"), filepath.Join(global, "sessions")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("global path %q not created: %v", path, err)
		}
	}
}

func TestEnsureDirsReportsFileConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := EnsureDirs(Config{SessionDir: filepath.Join(path, "child")})
	if err == nil || !strings.Contains(err.Error(), filepath.Join(path, "child")) {
		t.Fatalf("expected path-bearing directory error, got %v", err)
	}
}

func clearRuntimeConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"NOTCH_PROVIDER", "NOTCH_MODEL", "NOTCH_THINKING_LEVEL"} {
		t.Setenv(name, "")
	}
}

func writeJSON(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
