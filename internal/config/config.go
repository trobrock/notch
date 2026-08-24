// Package config loads and merges notch configuration files.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultProvider     = "anthropic"
	defaultModel        = "claude-sonnet-4-5"
	defaultMaxTokens    = 8192
	defaultSystemPrompt = "You are a coding agent. Help the user understand and modify their codebase."
	defaultTheme        = "dark"
	defaultThinking     = "medium"
)

type CompactionConfig struct {
	Enabled          *bool `json:"enabled,omitempty"`
	ReserveTokens    int   `json:"reserveTokens,omitempty"`
	KeepRecentTokens int   `json:"keepRecentTokens,omitempty"`
}

func (c *CompactionConfig) UnmarshalJSON(data []byte) error {
	var wire struct {
		Enabled          *bool `json:"enabled"`
		ReserveTokens    int   `json:"reserveTokens"`
		KeepRecentTokens int   `json:"keepRecentTokens"`
		ReserveSnake     int   `json:"reserve_tokens"`
		KeepRecentSnake  int   `json:"keep_recent_tokens"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	c.Enabled = wire.Enabled
	c.ReserveTokens = wire.ReserveTokens
	c.KeepRecentTokens = wire.KeepRecentTokens
	if c.ReserveTokens == 0 {
		c.ReserveTokens = wire.ReserveSnake
	}
	if c.KeepRecentTokens == 0 {
		c.KeepRecentTokens = wire.KeepRecentSnake
	}
	return nil
}

// Config is the configuration for the notch process. Provider credentials are
// deliberately not represented here; providers obtain credentials from their
// environment.
type Config struct {
	Provider          string            `json:"provider,omitempty"`
	Model             string            `json:"model,omitempty"`
	BaseURL           string            `json:"base_url,omitempty"`
	MaxTokens         int               `json:"max_tokens,omitempty"`
	SystemPrompt      string            `json:"system_prompt,omitempty"`
	MCPConfig         string            `json:"mcp_config,omitempty"`
	ExtensionDirs     []string          `json:"extension_dirs,omitempty"`
	SkillDirs         []string          `json:"skill_dirs,omitempty"`
	PromptDirs        []string          `json:"prompt_dirs,omitempty"`
	ThemeDirs         []string          `json:"theme_dirs,omitempty"`
	AgentSkillDirs    []string          `json:"-"`
	AgentCommandDirs  []string          `json:"-"`
	SessionDir        string            `json:"session_dir,omitempty"`
	AuthFile          string            `json:"auth_file,omitempty"`
	Theme             string            `json:"theme,omitempty"`
	ThinkingLevel     string            `json:"thinking_level,omitempty"`
	ContextWindow     int               `json:"context_window,omitempty"`
	ModelCache        string            `json:"model_cache,omitempty"`
	ModelRefreshHours int               `json:"model_refresh_hours,omitempty"`
	Compaction        *CompactionConfig `json:"compaction,omitempty"`
}

// Defaults returns the built-in configuration. home is the user's home
// directory and cwd is the project directory. NOTCH_HOME, when non-empty,
// replaces home/.notch as the per-user notch directory.
func Defaults(home, cwd string) Config {
	root := notchHome(home)
	projectRoot := filepath.Join(cwd, ".notch")
	enabled := true
	return Config{
		Provider:          defaultProvider,
		Model:             defaultModel,
		MaxTokens:         defaultMaxTokens,
		SystemPrompt:      defaultSystemPrompt,
		MCPConfig:         filepath.Join(root, "mcp.json"),
		ExtensionDirs:     uniquePaths(filepath.Join(root, "extensions"), filepath.Join(projectRoot, "extensions")),
		SkillDirs:         uniquePaths(filepath.Join(root, "skills"), filepath.Join(projectRoot, "skills")),
		PromptDirs:        uniquePaths(filepath.Join(root, "prompts"), filepath.Join(projectRoot, "prompts")),
		ThemeDirs:         uniquePaths(filepath.Join(root, "themes"), filepath.Join(projectRoot, "themes")),
		AgentSkillDirs:    uniquePaths(filepath.Join(home, ".agents", "skills"), filepath.Join(cwd, ".agents", "skills")),
		AgentCommandDirs:  uniquePaths(filepath.Join(home, ".agents", "commands"), filepath.Join(cwd, ".agents", "commands")),
		SessionDir:        filepath.Join(root, "sessions"),
		AuthFile:          filepath.Join(root, "auth.json"),
		Theme:             defaultTheme,
		ThinkingLevel:     defaultThinking,
		ModelCache:        filepath.Join(root, "models.json"),
		ModelRefreshHours: 24,
		Compaction:        &CompactionConfig{Enabled: &enabled, ReserveTokens: 16384, KeepRecentTokens: 20000},
	}
}

// Load loads the built-in defaults, then the per-user configuration, then the
// project configuration, and finally NOTCH_PROVIDER, NOTCH_MODEL, and
// NOTCH_THINKING_LEVEL. Missing files are not errors. A non-zero value in a
// later layer replaces the corresponding earlier value; directory lists are
// replaced as a whole when non-empty.
func Load(home, cwd string) (Config, error) {
	cfg := Defaults(home, cwd)
	root := notchHome(home)
	paths := []string{
		filepath.Join(root, "config.json"),
		filepath.Join(cwd, ".notch", "config.json"),
	}
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		layer, err := read(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return Config{}, err
		}
		merge(&cfg, layer)
	}
	applyEnvironment(&cfg)
	return cfg, nil
}

func applyEnvironment(cfg *Config) {
	if value := strings.TrimSpace(os.Getenv("NOTCH_PROVIDER")); value != "" {
		cfg.Provider = value
	}
	if value := strings.TrimSpace(os.Getenv("NOTCH_MODEL")); value != "" {
		cfg.Model = value
	}
	if value := strings.TrimSpace(os.Getenv("NOTCH_THINKING_LEVEL")); value != "" {
		cfg.ThinkingLevel = value
	}
}

func read(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	return cfg, nil
}

func merge(dst *Config, src Config) {
	if src.Provider != "" {
		dst.Provider = src.Provider
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.BaseURL != "" {
		dst.BaseURL = src.BaseURL
	}
	if src.MaxTokens != 0 {
		dst.MaxTokens = src.MaxTokens
	}
	if src.SystemPrompt != "" {
		dst.SystemPrompt = src.SystemPrompt
	}
	if src.MCPConfig != "" {
		dst.MCPConfig = src.MCPConfig
	}
	if len(src.ExtensionDirs) != 0 {
		dst.ExtensionDirs = append([]string(nil), src.ExtensionDirs...)
	}
	if len(src.SkillDirs) != 0 {
		dst.SkillDirs = append([]string(nil), src.SkillDirs...)
	}
	if len(src.PromptDirs) != 0 {
		dst.PromptDirs = append([]string(nil), src.PromptDirs...)
	}
	if len(src.ThemeDirs) != 0 {
		dst.ThemeDirs = append([]string(nil), src.ThemeDirs...)
	}
	if src.SessionDir != "" {
		dst.SessionDir = src.SessionDir
	}
	if src.AuthFile != "" {
		dst.AuthFile = src.AuthFile
	}
	if src.Theme != "" {
		dst.Theme = src.Theme
	}
	if src.ThinkingLevel != "" {
		dst.ThinkingLevel = src.ThinkingLevel
	}
	if src.ContextWindow > 0 {
		dst.ContextWindow = src.ContextWindow
	}
	if src.ModelCache != "" {
		dst.ModelCache = src.ModelCache
	}
	if src.ModelRefreshHours > 0 {
		dst.ModelRefreshHours = src.ModelRefreshHours
	}
	if src.Compaction != nil {
		if dst.Compaction == nil {
			dst.Compaction = &CompactionConfig{}
		}
		if src.Compaction.Enabled != nil {
			value := *src.Compaction.Enabled
			dst.Compaction.Enabled = &value
		}
		if src.Compaction.ReserveTokens > 0 {
			dst.Compaction.ReserveTokens = src.Compaction.ReserveTokens
		}
		if src.Compaction.KeepRecentTokens > 0 {
			dst.Compaction.KeepRecentTokens = src.Compaction.KeepRecentTokens
		}
	}
}

// EnsureDirs creates all configured extension, skill, prompt, theme, and
// session directories. Empty entries are ignored.
func EnsureDirs(cfg Config) error {
	dirs := make([]string, 0, len(cfg.ExtensionDirs)+len(cfg.SkillDirs)+len(cfg.PromptDirs)+len(cfg.ThemeDirs)+1)
	dirs = append(dirs, cfg.ExtensionDirs...)
	dirs = append(dirs, cfg.SkillDirs...)
	dirs = append(dirs, cfg.PromptDirs...)
	dirs = append(dirs, cfg.ThemeDirs...)
	dirs = append(dirs, cfg.SessionDir)
	seen := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		clean := filepath.Clean(dir)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create config directory %q: %w", dir, err)
		}
	}
	return nil
}

// SkillDiscoveryDirs returns shared global, configured, then shared project
// directories. Later entries win on duplicate resource names.
func (c Config) SkillDiscoveryDirs() []string {
	return layeredDiscoveryDirs(c.AgentSkillDirs, c.SkillDirs)
}

// PromptDiscoveryDirs returns .agents/commands and configured prompt directories.
func (c Config) PromptDiscoveryDirs() []string {
	return layeredDiscoveryDirs(c.AgentCommandDirs, c.PromptDirs)
}

func layeredDiscoveryDirs(shared, configured []string) []string {
	var paths []string
	if len(shared) != 0 {
		paths = append(paths, shared[0])
	}
	paths = append(paths, configured...)
	if len(shared) > 1 {
		paths = append(paths, shared[1:]...)
	}
	return uniquePaths(paths...)
}

// EnsureDirs creates the directories named by c.
func (c Config) EnsureDirs() error { return EnsureDirs(c) }

// HomeDir returns the effective Notch data directory for a user home.
func HomeDir(home string) string { return notchHome(home) }

func notchHome(home string) string {
	if value := os.Getenv("NOTCH_HOME"); value != "" {
		return filepath.Clean(value)
	}
	return filepath.Join(home, ".notch")
}

func uniquePaths(paths ...string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		if !seen[clean] {
			seen[clean] = true
			out = append(out, path)
		}
	}
	return out
}
