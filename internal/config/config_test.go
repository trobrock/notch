package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
	if cfg.MCPConfig != filepath.Join(root, "mcp.json") || cfg.SessionDir != filepath.Join(root, "sessions") || cfg.ModelCache != filepath.Join(root, "models.json") || cfg.ModelRefreshHours != 24 {
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
	if cfg.SessionDir != filepath.Join(custom, "sessions") || cfg.MCPConfig != filepath.Join(custom, "mcp.json") {
		t.Fatalf("NOTCH_HOME not respected: %+v", cfg)
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
	if cfg.SystemPrompt != "global prompt" || cfg.MCPConfig != "project-mcp.json" || cfg.SessionDir != "global-sessions" {
		t.Fatalf("scalar inheritance failed: %+v", cfg)
	}
	if cfg.Theme != "dracula" || cfg.ThinkingLevel != "high" || cfg.MouseCapture == nil || *cfg.MouseCapture || cfg.ContextWindow != 99999 || cfg.ModelCache != "custom-models.json" || cfg.ModelRefreshHours != 12 || cfg.Compaction == nil || cfg.Compaction.Enabled == nil || *cfg.Compaction.Enabled || cfg.Compaction.ReserveTokens != 1000 || cfg.Compaction.KeepRecentTokens != 3000 {
		t.Fatalf("theme/thinking/compaction merge failed: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.ExtensionDirs, []string{"project-ext"}) ||
		!reflect.DeepEqual(cfg.SkillDirs, []string{"global-skill"}) ||
		!reflect.DeepEqual(cfg.PromptDirs, []string{"global-prompt"}) ||
		!reflect.DeepEqual(cfg.ThemeDirs, []string{"project-theme"}) {
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
