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
	if cfg.MCPConfig != filepath.Join(root, "mcp.json") || cfg.SessionDir != filepath.Join(root, "sessions") {
		t.Fatalf("defaults use wrong home: %+v", cfg)
	}
	wantExtensions := []string{filepath.Join(root, "extensions"), filepath.Join(cwd, ".notch", "extensions")}
	if !reflect.DeepEqual(cfg.ExtensionDirs, wantExtensions) {
		t.Fatalf("extension dirs = %#v, want %#v", cfg.ExtensionDirs, wantExtensions)
	}

	custom := filepath.Join(t.TempDir(), "custom-notch")
	t.Setenv("NOTCH_HOME", custom)
	cfg = Defaults(home, cwd)
	if cfg.SessionDir != filepath.Join(custom, "sessions") || cfg.MCPConfig != filepath.Join(custom, "mcp.json") {
		t.Fatalf("NOTCH_HOME not respected: %+v", cfg)
	}
}

func TestLoadMergesUserThenProject(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("NOTCH_HOME", "")
	writeJSON(t, filepath.Join(home, ".notch", "config.json"), `{
		"provider":"openai", "model":"global-model", "base_url":"https://global.test",
		"max_tokens":123, "system_prompt":"global prompt", "extension_dirs":["global-ext"],
		"skill_dirs":["global-skill"], "prompt_dirs":["global-prompt"], "session_dir":"global-sessions"
	}`)
	writeJSON(t, filepath.Join(cwd, ".notch", "config.json"), `{
		"model":"project-model", "max_tokens":456, "mcp_config":"project-mcp.json",
		"extension_dirs":["project-ext"], "provider":"", "prompt_dirs":[]
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
	if !reflect.DeepEqual(cfg.ExtensionDirs, []string{"project-ext"}) ||
		!reflect.DeepEqual(cfg.SkillDirs, []string{"global-skill"}) ||
		!reflect.DeepEqual(cfg.PromptDirs, []string{"global-prompt"}) {
		t.Fatalf("directory merge failed: %+v", cfg)
	}
}

func TestLoadMissingAndMalformed(t *testing.T) {
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

func TestEnsureDirs(t *testing.T) {
	root := t.TempDir()
	dirs := []string{
		filepath.Join(root, "extensions", "nested"),
		filepath.Join(root, "skills"),
		filepath.Join(root, "prompts"),
		filepath.Join(root, "sessions"),
	}
	cfg := Config{
		ExtensionDirs: []string{dirs[0], dirs[0], ""},
		SkillDirs:     []string{dirs[1]},
		PromptDirs:    []string{dirs[2]},
		SessionDir:    dirs[3],
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

func writeJSON(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
