package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultSystemPromptGuidesCodebaseExploration(t *testing.T) {
	cfg, err := Defaults(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"likely to save parent context", "Prefer direct grep", "avoid delegation", "exactly one of task", "never provide both"} {
		if !strings.Contains(cfg.SystemPrompt, text) {
			t.Fatalf("default system prompt missing %q: %q", text, cfg.SystemPrompt)
		}
	}
}

func TestDefaultsUseXDGSplitAndIgnoreLegacyHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	cwd := filepath.Join(t.TempDir(), "project")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("NOTCH_HOME", filepath.Join(t.TempDir(), "ignored"))

	cfg, err := Defaults(home, cwd)
	if err != nil {
		t.Fatal(err)
	}
	configRoot := filepath.Join(home, ".config", "notch")
	dataRoot := filepath.Join(home, ".local", "share", "notch")
	if cfg.MCPConfig != filepath.Join(configRoot, "mcp.json") || cfg.MCPAuthFile != filepath.Join(dataRoot, "mcp-auth.json") || cfg.AuthFile != filepath.Join(dataRoot, "auth.json") || cfg.SessionDir != filepath.Join(dataRoot, "sessions") || cfg.ModelCache != filepath.Join(dataRoot, "models.json") {
		t.Fatalf("defaults use wrong XDG roots: %+v", cfg)
	}
	wantExtensions := []string{filepath.Join(configRoot, "extensions"), filepath.Join(cwd, ".notch", "extensions")}
	if !reflect.DeepEqual(cfg.ExtensionDirs, wantExtensions) {
		t.Fatalf("extension dirs = %#v, want %#v", cfg.ExtensionDirs, wantExtensions)
	}
	wantAgentSkills := []string{filepath.Join(home, ".agents", "skills"), filepath.Join(cwd, ".agents", "skills")}
	wantSkillOrder := []string{wantAgentSkills[0], filepath.Join(configRoot, "skills"), filepath.Join(cwd, ".notch", "skills"), wantAgentSkills[1]}
	if !reflect.DeepEqual(cfg.SkillDiscoveryDirs(), wantSkillOrder) {
		t.Fatalf("skill discovery order = %#v, want %#v", cfg.SkillDiscoveryDirs(), wantSkillOrder)
	}
	if _, err := os.Stat(filepath.Join(home, ".notch")); !os.IsNotExist(err) {
		t.Fatalf("legacy ~/.notch unexpectedly used: %v", err)
	}
}

func TestXDGOverridesAndRelativeRejection(t *testing.T) {
	home := t.TempDir()
	configBase, dataBase := filepath.Join(t.TempDir(), "config"), filepath.Join(t.TempDir(), "data")
	t.Setenv("XDG_CONFIG_HOME", configBase)
	t.Setenv("XDG_DATA_HOME", dataBase)
	cfg, err := Defaults(home, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPConfig != filepath.Join(configBase, "notch", "mcp.json") || cfg.AuthFile != filepath.Join(dataBase, "notch", "auth.json") {
		t.Fatalf("XDG overrides not applied: %+v", cfg)
	}
	for _, variable := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
		t.Setenv(variable, "relative/path")
		if _, err := LoadGlobal(home); err == nil || !strings.Contains(err.Error(), variable+" must be an absolute path") {
			t.Fatalf("%s relative error = %v", variable, err)
		}
		t.Setenv(variable, "")
	}
}

func TestLoadMergesUserThenProject(t *testing.T) {
	clearRuntimeConfigEnv(t)
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("NOTCH_HOME", "")
	writeJSON(t, filepath.Join(home, ".config", "notch", "config.json"), `{
		"provider":"openai", "model":"global-model", "base_url":"https://global.test",
		"max_tokens":123, "system_prompt":"global prompt", "extension_dirs":["global-ext"],
		"skill_dirs":["global-skill"], "prompt_dirs":["global-prompt"], "theme_dirs":["global-theme"], "session_dir":"global-sessions",
		"theme":"dracula", "thinking_level":"low", "mouse":false, "context_window":99999, "model_cache":"custom-models.json", "model_refresh_hours":12,
		"presets":{"f1":{"provider":"anthropic","model":"global-preset","thinking_level":"low"}},
		"compaction":{"enabled":false,"reserve_tokens":1000,"keep_recent_tokens":2000}
	}`)
	writeJSON(t, filepath.Join(cwd, ".notch", "config.json"), `{
		"model":"project-model", "max_tokens":456, "mcp_config":"project-mcp.json",
		"extension_dirs":["project-ext"], "provider":"", "prompt_dirs":[], "theme_dirs":["project-theme"],
		"thinking_level":"high", "presets":{"F2":{"provider":"openai-codex","model":"project-preset","thinking_level":"high"}},
		"compaction":{"keep_recent_tokens":3000}
	}`)

	cfg, err := Load(home, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "openai" || cfg.Model != "project-model" || cfg.BaseURL != "https://global.test" || cfg.MaxTokens != 456 {
		t.Fatalf("scalar merge failed: %+v", cfg)
	}
	if cfg.SystemPrompt != "global prompt" || cfg.MCPConfig != filepath.Join(cwd, "project-mcp.json") || cfg.SessionDir != filepath.Join(home, ".local", "share", "notch", "sessions") {
		t.Fatalf("scalar inheritance failed: %+v", cfg)
	}
	if cfg.Theme != "dracula" || cfg.ThinkingLevel != "high" || cfg.MouseCapture == nil || *cfg.MouseCapture || cfg.ContextWindow != 99999 || cfg.ModelCache != filepath.Join(home, ".local", "share", "notch", "models.json") || cfg.ModelRefreshHours != 12 || cfg.Compaction == nil || cfg.Compaction.Enabled == nil || *cfg.Compaction.Enabled || cfg.Compaction.ReserveTokens != 1000 || cfg.Compaction.KeepRecentTokens != 3000 {
		t.Fatalf("theme/thinking/compaction merge failed: %+v", cfg)
	}
	if len(cfg.Presets) != 2 || cfg.Presets["f1"].Model != "global-preset" || cfg.Presets["f2"].Model != "project-preset" {
		t.Fatalf("preset merge failed: %+v", cfg.Presets)
	}
	if !reflect.DeepEqual(cfg.ExtensionDirs, []string{filepath.Join(cwd, "project-ext")}) ||
		!reflect.DeepEqual(cfg.SkillDirs, []string{filepath.Join(home, ".config", "notch", "global-skill")}) ||
		!reflect.DeepEqual(cfg.PromptDirs, []string{filepath.Join(home, ".config", "notch", "global-prompt")}) ||
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
	writeJSON(t, filepath.Join(home, ".config", "notch", "config.json"), `{bad`)
	_, err := Load(home, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), filepath.Join(home, ".config", "notch", "config.json")) {
		t.Fatalf("expected path-bearing parse error, got %v", err)
	}
}

func TestLoadIgnoresNotchHomeAndHasNoLegacyFallback(t *testing.T) {
	clearRuntimeConfigEnv(t)
	home, cwd, custom := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("NOTCH_HOME", custom)
	writeJSON(t, filepath.Join(home, ".notch", "config.json"), `{"model":"legacy"}`)
	writeJSON(t, filepath.Join(home, ".config", "notch", "config.json"), `{"model":"xdg"}`)
	writeJSON(t, filepath.Join(custom, "config.json"), `{"model":"notch-home"}`)
	cfg, err := Load(home, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "xdg" {
		t.Fatalf("model = %q, want XDG config with legacy and NOTCH_HOME ignored", cfg.Model)
	}
}

func TestLoadDoesNotFallbackToLegacyConfig(t *testing.T) {
	clearRuntimeConfigEnv(t)
	home := t.TempDir()
	writeJSON(t, filepath.Join(home, ".notch", "config.json"), `{"model":"legacy"}`)
	cfg, err := LoadGlobal(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model == "legacy" {
		t.Fatal("loaded legacy ~/.notch/config.json")
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
	writeJSON(t, filepath.Join(home, ".config", "notch", "config.json"), `{"model":"global","extension_dirs":["global-ext"]}`)
	writeJSON(t, filepath.Join(root, ".notch", "config.json"), `{"model":"project","extension_dirs":["project-ext"]}`)

	cfg, err := LoadWorkspace(home, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "global" || !reflect.DeepEqual(cfg.ExtensionDirs, []string{filepath.Join(home, ".config", "notch", "global-ext")}) {
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

func TestGlobalAndProjectJSONCannotConfigureFixedDataPaths(t *testing.T) {
	clearRuntimeConfigEnv(t)
	home, root := t.TempDir(), t.TempDir()
	writeJSON(t, filepath.Join(home, ".config", "notch", "config.json"), `{
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
	if cfg.BaseURL != "https://global.test" || cfg.AuthFile != filepath.Join(home, ".local", "share", "notch", "auth.json") || cfg.MCPAuthFile != filepath.Join(home, ".local", "share", "notch", "mcp-auth.json") || cfg.SessionDir != filepath.Join(home, ".local", "share", "notch", "sessions") || cfg.ModelCache != filepath.Join(home, ".local", "share", "notch", "models.json") {
		t.Fatalf("project overrode sensitive global fields: %+v", cfg)
	}
}

func TestLoadWorkspaceClearsGlobalBaseURLWhenProjectChangesProvider(t *testing.T) {
	clearRuntimeConfigEnv(t)
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("NOTCH_HOME", "")
	writeJSON(t, filepath.Join(home, ".config", "notch", "config.json"), `{"provider":"openai","base_url":"https://global.test"}`)
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
	writeJSON(t, filepath.Join(home, ".config", "notch", "config.json"), `{"auth_file":"global-auth"}`)
	cfg, err := LoadGlobal(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthFile != filepath.Join(home, ".local", "share", "notch", "auth.json") {
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
	if info, err := os.Stat(dirs[4]); err != nil {
		t.Errorf("stat private session directory: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Errorf("private session directory mode = %v", info.Mode().Perm())
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
		configRoot:    global,
		dataRoot:      global,
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
	for _, name := range []string{"NOTCH_PROVIDER", "NOTCH_MODEL", "NOTCH_THINKING_LEVEL", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "NOTCH_HOME"} {
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
