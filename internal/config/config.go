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
	defaultSystemPrompt = `You are a coding agent. Help the user understand and modify their codebase.

Delegate broad codebase discovery or multi-file flow tracing only when doing so is likely to save parent context or parallelize independent work. Prefer direct grep, find, read, and ls calls for focused work, and avoid delegation when startup and duplicated context would cost more than a few direct tool calls. Bring only concise findings into the main context. When calling explore_codebase, always provide a tasks array: one item for one focused question or multiple independent items to run in parallel. Normally omit model so Notch uses the configured explore model or the current parent model. Never invent a model ID; if an explore model is unavailable, call list_models for that provider and retry once with the closest available model in the same family and capability tier.`
	defaultTheme    = "dark"
	defaultThinking = "medium"
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

type PresetConfig struct {
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	ThinkingLevel string `json:"thinking_level,omitempty"`
}

// Config is the configuration for the notch process. Provider credentials are
// deliberately not represented here; providers obtain credentials from their
// environment.
type Config struct {
	Provider          string                  `json:"provider,omitempty"`
	Model             string                  `json:"model,omitempty"`
	ExploreModel      string                  `json:"explore_model,omitempty"`
	BaseURL           string                  `json:"base_url,omitempty"`
	MaxTokens         int                     `json:"max_tokens,omitempty"`
	SystemPrompt      string                  `json:"system_prompt,omitempty"`
	MCPConfig         string                  `json:"mcp_config,omitempty"`
	ExtensionDirs     []string                `json:"extension_dirs,omitempty"`
	SkillDirs         []string                `json:"skill_dirs,omitempty"`
	PromptDirs        []string                `json:"prompt_dirs,omitempty"`
	ThemeDirs         []string                `json:"theme_dirs,omitempty"`
	AgentSkillDirs    []string                `json:"-"`
	AgentCommandDirs  []string                `json:"-"`
	SessionDir        string                  `json:"-"`
	AuthFile          string                  `json:"-"`
	MCPAuthFile       string                  `json:"-"`
	Theme             string                  `json:"theme,omitempty"`
	ThinkingLevel     string                  `json:"thinking_level,omitempty"`
	CacheRetention    string                  `json:"cache_retention,omitempty"`
	Presets           map[string]PresetConfig `json:"presets,omitempty"`
	MouseCapture      *bool                   `json:"mouse,omitempty"`
	ContextWindow     int                     `json:"context_window,omitempty"`
	ModelCache        string                  `json:"-"`
	ModelRefreshHours int                     `json:"model_refresh_hours,omitempty"`
	Compaction        *CompactionConfig       `json:"compaction,omitempty"`
	configRoot        string
	dataRoot          string
}

// Defaults returns the built-in configuration. home is the user's home
// directory and cwd is the project directory. XDG_CONFIG_HOME and
// XDG_DATA_HOME, when non-empty, must be absolute.
func Defaults(home, cwd string) (Config, error) {
	return defaults(home, cwd, true)
}

func defaults(home, cwd string, includeProject bool) (Config, error) {
	configRoot, dataRoot, err := Roots(home)
	if err != nil {
		return Config{}, err
	}
	projectRoot := filepath.Join(cwd, ".notch")
	extensionDirs := []string{filepath.Join(configRoot, "extensions")}
	skillDirs := []string{filepath.Join(configRoot, "skills")}
	promptDirs := []string{filepath.Join(configRoot, "prompts")}
	themeDirs := []string{filepath.Join(configRoot, "themes")}
	agentSkillDirs := []string{filepath.Join(home, ".agents", "skills")}
	agentCommandDirs := []string{filepath.Join(home, ".agents", "commands")}
	if includeProject {
		extensionDirs = uniquePaths(append(extensionDirs, filepath.Join(projectRoot, "extensions"))...)
		skillDirs = uniquePaths(append(skillDirs, filepath.Join(projectRoot, "skills"))...)
		promptDirs = uniquePaths(append(promptDirs, filepath.Join(projectRoot, "prompts"))...)
		themeDirs = uniquePaths(append(themeDirs, filepath.Join(projectRoot, "themes"))...)
		agentSkillDirs = uniquePaths(append(agentSkillDirs, filepath.Join(cwd, ".agents", "skills"))...)
		agentCommandDirs = uniquePaths(append(agentCommandDirs, filepath.Join(cwd, ".agents", "commands"))...)
	}
	enabled := true
	mouseEnabled := true
	return Config{
		Provider:          defaultProvider,
		Model:             defaultModel,
		MaxTokens:         defaultMaxTokens,
		SystemPrompt:      defaultSystemPrompt,
		MCPConfig:         filepath.Join(configRoot, "mcp.json"),
		ExtensionDirs:     extensionDirs,
		SkillDirs:         skillDirs,
		PromptDirs:        promptDirs,
		ThemeDirs:         themeDirs,
		AgentSkillDirs:    agentSkillDirs,
		AgentCommandDirs:  agentCommandDirs,
		SessionDir:        filepath.Join(dataRoot, "sessions"),
		AuthFile:          filepath.Join(dataRoot, "auth.json"),
		MCPAuthFile:       filepath.Join(dataRoot, "mcp-auth.json"),
		Theme:             defaultTheme,
		ThinkingLevel:     defaultThinking,
		CacheRetention:    "short",
		MouseCapture:      &mouseEnabled,
		ModelCache:        filepath.Join(dataRoot, "models.json"),
		ModelRefreshHours: 24,
		Compaction:        &CompactionConfig{Enabled: &enabled, ReserveTokens: 16384, KeepRecentTokens: 20000},
		configRoot:        configRoot,
		dataRoot:          dataRoot,
	}, nil
}

// Load loads the built-in defaults, then the per-user configuration, then the
// project configuration, and finally NOTCH_PROVIDER, NOTCH_MODEL,
// NOTCH_EXPLORE_MODEL, and NOTCH_THINKING_LEVEL. It preserves the historical API and treats project
// inputs as enabled; callers handling an untrusted workspace should use
// LoadWorkspace.
func Load(home, cwd string) (Config, error) {
	return LoadWorkspace(home, cwd, true)
}

// LoadGlobal loads only built-in and per-user configuration. It never reads
// project configuration or adds project discovery directories.
func LoadGlobal(home string) (Config, error) {
	return load(home, "", false)
}

// LoadWorkspace loads global configuration and, when trusted is true, project
// configuration and project discovery directories rooted at workspaceRoot.
// Security-sensitive paths and endpoints are always taken from built-in or
// global configuration, never project configuration.
func LoadWorkspace(home, workspaceRoot string, trusted bool) (Config, error) {
	return load(home, workspaceRoot, trusted)
}

func load(home, workspaceRoot string, includeProject bool) (Config, error) {
	cfg, err := defaults(home, workspaceRoot, includeProject)
	if err != nil {
		return Config{}, err
	}
	globalPath := filepath.Join(cfg.configRoot, "config.json")
	if _, err := os.Lstat(globalPath); err == nil {
		layer, err := read(globalPath)
		if err != nil {
			return Config{}, err
		}
		resolveGlobalPaths(&layer, cfg.configRoot)
		merge(&cfg, layer)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("inspect config %q: %w", globalPath, err)
	}
	if includeProject {
		projectNotch := filepath.Join(workspaceRoot, ".notch")
		projectMCP := filepath.Join(projectNotch, "mcp.json")
		if _, statErr := os.Stat(projectMCP); statErr == nil {
			cfg.MCPConfig = projectMCP
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return Config{}, fmt.Errorf("inspect project MCP config %q: %w", projectMCP, statErr)
		}
		projectPath := filepath.Join(projectNotch, "config.json")
		if filepath.Clean(projectPath) != filepath.Clean(globalPath) {
			if _, err := os.Lstat(projectPath); err == nil {
				layer, err := read(projectPath)
				if err != nil {
					return Config{}, err
				}
				resolveProjectPaths(&layer, workspaceRoot)
				mergeProject(&cfg, layer)
			} else if !errors.Is(err, os.ErrNotExist) {
				return Config{}, fmt.Errorf("inspect config %q: %w", projectPath, err)
			}
		}
	}
	applyEnvironment(&cfg)
	return cfg, nil
}

func resolveProjectPaths(cfg *Config, workspaceRoot string) {
	resolveConfigPaths(cfg, workspaceRoot)
}

func resolveGlobalPaths(cfg *Config, configRoot string) {
	resolveConfigPaths(cfg, configRoot)
}

func resolveConfigPaths(cfg *Config, root string) {
	cfg.MCPConfig = resolveRelative(root, cfg.MCPConfig)
	cfg.ExtensionDirs = resolveRelativePaths(root, cfg.ExtensionDirs)
	cfg.SkillDirs = resolveRelativePaths(root, cfg.SkillDirs)
	cfg.PromptDirs = resolveRelativePaths(root, cfg.PromptDirs)
	cfg.ThemeDirs = resolveRelativePaths(root, cfg.ThemeDirs)
}

func resolveRelativePaths(root string, paths []string) []string {
	resolved := make([]string, 0, len(paths))
	for _, path := range paths {
		resolved = append(resolved, resolveRelative(root, path))
	}
	return uniquePaths(resolved...)
}

func resolveRelative(root, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

func applyEnvironment(cfg *Config) {
	if value := strings.TrimSpace(os.Getenv("NOTCH_PROVIDER")); value != "" {
		cfg.Provider = value
	}
	if value := strings.TrimSpace(os.Getenv("NOTCH_MODEL")); value != "" {
		cfg.Model = value
	}
	if value := strings.TrimSpace(os.Getenv("NOTCH_EXPLORE_MODEL")); value != "" {
		cfg.ExploreModel = value
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

func mergeProject(dst *Config, src Config) {
	// These values choose remote endpoints or credential/session/cache files and
	// therefore remain global-only even after the workspace is trusted.
	providerChanged := src.Provider != "" && src.Provider != dst.Provider
	src.BaseURL = ""
	src.AuthFile = ""
	src.MCPAuthFile = ""
	src.SessionDir = ""
	src.ModelCache = ""
	if providerChanged {
		// A global custom endpoint is provider-specific. Do not carry it across a
		// project provider override; the newly selected provider should use its
		// official default endpoint.
		dst.BaseURL = ""
	}
	merge(dst, src)
}

func merge(dst *Config, src Config) {
	if src.Provider != "" {
		dst.Provider = src.Provider
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if value := strings.TrimSpace(src.ExploreModel); value != "" {
		dst.ExploreModel = value
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
	if src.Theme != "" {
		dst.Theme = src.Theme
	}
	if src.ThinkingLevel != "" {
		dst.ThinkingLevel = src.ThinkingLevel
	}
	if src.CacheRetention != "" {
		dst.CacheRetention = strings.ToLower(strings.TrimSpace(src.CacheRetention))
	}
	if len(src.Presets) != 0 {
		if dst.Presets == nil {
			dst.Presets = make(map[string]PresetConfig, len(src.Presets))
		}
		for key, preset := range src.Presets {
			dst.Presets[strings.ToLower(strings.TrimSpace(key))] = preset
		}
	}
	if src.MouseCapture != nil {
		value := *src.MouseCapture
		dst.MouseCapture = &value
	}
	if src.ContextWindow > 0 {
		dst.ContextWindow = src.ContextWindow
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

// EnsureDirs creates all configured extension, skill, prompt, and theme
// directories, plus the private session directory. Empty entries are ignored.
func EnsureDirs(cfg Config) error {
	dirs := make([]string, 0, len(cfg.ExtensionDirs)+len(cfg.SkillDirs)+len(cfg.PromptDirs)+len(cfg.ThemeDirs))
	dirs = append(dirs, cfg.ExtensionDirs...)
	dirs = append(dirs, cfg.SkillDirs...)
	dirs = append(dirs, cfg.PromptDirs...)
	dirs = append(dirs, cfg.ThemeDirs...)
	seen := make(map[string]bool, len(dirs)+1)
	for _, dir := range dirs {
		if err := ensureDir(dir, 0o755, seen); err != nil {
			return err
		}
	}
	return ensureDir(cfg.SessionDir, 0o700, seen)
}

func ensureDir(dir string, mode os.FileMode, seen map[string]bool) error {
	if dir == "" {
		return nil
	}
	clean := filepath.Clean(dir)
	if seen[clean] {
		return nil
	}
	seen[clean] = true
	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("create config directory %q: %w", dir, err)
	}
	if mode == 0o700 {
		if err := os.Chmod(dir, mode); err != nil {
			return fmt.Errorf("secure private data directory %q: %w", dir, err)
		}
	}
	return nil
}

// EnsureGlobalDirs creates only configured paths below the XDG config root,
// plus the fixed private session directory below the XDG data root. This avoids
// creating project or arbitrary configured paths merely by inspecting them.
func (c Config) EnsureGlobalDirs() error {
	if c.configRoot == "" || c.dataRoot == "" {
		return c.EnsureDirs()
	}
	if err := ensureDir(c.dataRoot, 0o700, make(map[string]bool)); err != nil {
		return err
	}
	filtered := c
	filtered.ExtensionDirs = pathsWithin(c.configRoot, c.ExtensionDirs)
	filtered.SkillDirs = pathsWithin(c.configRoot, c.SkillDirs)
	filtered.PromptDirs = pathsWithin(c.configRoot, c.PromptDirs)
	filtered.ThemeDirs = pathsWithin(c.configRoot, c.ThemeDirs)
	if !pathWithin(c.dataRoot, c.SessionDir) {
		filtered.SessionDir = ""
	}
	return EnsureDirs(filtered)
}

func pathsWithin(root string, paths []string) []string {
	var filtered []string
	for _, path := range paths {
		if pathWithin(root, path) {
			filtered = append(filtered, path)
		}
	}
	return filtered
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
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

// ConfigDir returns the strict per-user Notch configuration root.
func ConfigDir(home string) (string, error) {
	configRoot, _, err := Roots(home)
	return configRoot, err
}

// DataDir returns the strict per-user Notch private data root.
func DataDir(home string) (string, error) {
	_, dataRoot, err := Roots(home)
	return dataRoot, err
}

// Roots resolves the XDG configuration and data roots. NOTCH_HOME is
// intentionally ignored; there is no legacy ~/.notch fallback or migration.
func Roots(home string) (string, string, error) {
	configBase, err := xdgBase("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err != nil {
		return "", "", err
	}
	dataBase, err := xdgBase("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	if err != nil {
		return "", "", err
	}
	return filepath.Join(configBase, "notch"), filepath.Join(dataBase, "notch"), nil
}

func xdgBase(name, fallback string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		value = fallback
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must be an absolute path, got %q", name, value)
	}
	return filepath.Clean(value), nil
}

func uniquePaths(paths ...string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		clean := filepath.Clean(path)
		if !seen[clean] {
			seen[clean] = true
			out = append(out, path)
		}
	}
	return out
}
