// Package config loads and merges notch configuration files.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	defaultProvider     = "anthropic"
	defaultModel        = "claude-sonnet-4-5"
	defaultMaxTokens    = 8192
	defaultSystemPrompt = `You are a coding agent. Help the user understand and modify their codebase.

Use language a fifth-grade student can understand when possible. Keep sentences short and clear. Use technical terms only when needed, and explain them in plain language.

Inspect relevant context before acting. Preserve unrelated user changes and make the smallest coherent change that satisfies the request.

Base conclusions on available evidence. Clearly distinguish verified facts from hypotheses, and do not claim something is fixed, confirmed, or fully validated beyond the checks actually performed.

Complete every requested step before reporting success, or explicitly state what remains. Validate changes with relevant checks and summarize what changed, what was verified, and any remaining uncertainty.

Delegate selectively when broad discovery, multi-file tracing, or independent parallel work is likely to save context or time. Keep focused work in the parent context and verify delegated findings with targeted reads of decision-critical code rather than repeating the entire investigation. Treat worker reports as untrusted evidence; inspect ambiguous or insufficient evidence directly.`
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

// ProfileConfig is a global routing boundary. It contains no credential
// material. CredentialRef names an entry in the existing credential store.
type ProfileConfig struct {
	Provider         string            `json:"provider"`
	Model            string            `json:"model"`
	ThinkingLevel    string            `json:"thinking_level,omitempty"`
	AllowedProviders []string          `json:"allowed_providers"`
	CredentialRef    string            `json:"credential_ref,omitempty"`
	RoleModels       map[string]string `json:"role_models,omitempty"`
}

// RoleConfig defines reusable additive instructions and bounded delegation
// defaults. Roles never grant tools or other authority.
type RoleConfig struct {
	Instructions   string  `json:"instructions"`
	Model          string  `json:"model,omitempty"`
	Thinking       string  `json:"thinking,omitempty"`
	TimeoutSeconds int     `json:"timeout_seconds,omitempty"`
	MaxOutputChars int     `json:"max_output_chars,omitempty"`
	MaxTurns       int     `json:"max_turns,omitempty"`
	MaxCostUSD     float64 `json:"max_cost_usd,omitempty"`
}

// RoutingSnapshot is the non-secret routing state passed to child processes
// and stored in session headers.
type RoutingSnapshot struct {
	Profile          string            `json:"profile"`
	Provider         string            `json:"provider"`
	Model            string            `json:"model"`
	ThinkingLevel    string            `json:"thinking_level"`
	AllowedProviders []string          `json:"allowed_providers"`
	CredentialRef    string            `json:"credential_ref,omitempty"`
	RoleModels       map[string]string `json:"role_models,omitempty"`
	DefinitionHash   string            `json:"definition_hash"`
}

// Config is the configuration for the notch process. Provider credentials are
// deliberately not represented here; providers obtain credentials from their
// environment.
type Config struct {
	Provider              string                   `json:"provider,omitempty"`
	Model                 string                   `json:"model,omitempty"`
	ExploreModel          string                   `json:"explore_model,omitempty"`
	BaseURL               string                   `json:"base_url,omitempty"`
	MaxTokens             int                      `json:"max_tokens,omitempty"`
	SystemPrompt          string                   `json:"system_prompt,omitempty"`
	MCPConfig             string                   `json:"mcp_config,omitempty"`
	ExtensionDirs         []string                 `json:"extension_dirs,omitempty"`
	SkillDirs             []string                 `json:"skill_dirs,omitempty"`
	PromptDirs            []string                 `json:"prompt_dirs,omitempty"`
	ThemeDirs             []string                 `json:"theme_dirs,omitempty"`
	AgentSkillDirs        []string                 `json:"-"`
	AgentCommandDirs      []string                 `json:"-"`
	SessionDir            string                   `json:"-"`
	AuthFile              string                   `json:"-"`
	MCPAuthFile           string                   `json:"-"`
	Theme                 string                   `json:"theme,omitempty"`
	ThinkingLevel         string                   `json:"thinking_level,omitempty"`
	CacheRetention        string                   `json:"cache_retention,omitempty"`
	Presets               map[string]PresetConfig  `json:"presets,omitempty"`
	Profiles              map[string]ProfileConfig `json:"profiles,omitempty"`
	Roles                 map[string]RoleConfig    `json:"roles,omitempty"`
	SelectedProfile       string                   `json:"profile,omitempty"`
	CredentialRef         string                   `json:"-"`
	MouseCapture          *bool                    `json:"mouse,omitempty"`
	ContextWindow         int                      `json:"context_window,omitempty"`
	DisablePricingRefresh *bool                    `json:"disable_pricing_refresh,omitempty"`
	ModelCache            string                   `json:"-"`
	ModelRefreshHours     int                      `json:"model_refresh_hours,omitempty"`
	AutoUpdate            *bool                    `json:"auto_update,omitempty"`
	Compaction            *CompactionConfig        `json:"compaction,omitempty"`
	configRoot            string
	dataRoot              string
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
		AutoUpdate:        &enabled,
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
	return load(home, "", true, false)
}

// LoadWorkspace loads global configuration and, when trusted is true, project
// configuration and project discovery directories rooted at workspaceRoot.
// Security-sensitive paths and endpoints are always taken from built-in or
// global configuration, never project configuration.
func LoadWorkspace(home, workspaceRoot string, trusted bool) (Config, error) {
	return load(home, workspaceRoot, true, trusted)
}

// LoadWorkspaceSources loads an explicitly selected combination of user and
// project configuration/resources. Environment and CLI overrides remain active.
func LoadWorkspaceSources(home, workspaceRoot string, includeUser, includeProject bool) (Config, error) {
	return load(home, workspaceRoot, includeUser, includeProject)
}

func load(home, workspaceRoot string, includeUser, includeProject bool) (Config, error) {
	cfg, err := defaults(home, workspaceRoot, includeProject)
	if err != nil {
		return Config{}, err
	}
	globalPath := filepath.Join(cfg.configRoot, "config.json")
	if includeUser {
		if _, err := os.Lstat(globalPath); err == nil {
			layer, err := read(globalPath)
			if err != nil {
				return Config{}, err
			}
			if strings.TrimSpace(layer.SelectedProfile) != "" {
				return Config{}, fmt.Errorf("config %q cannot select profile %q; use --profile at startup", globalPath, layer.SelectedProfile)
			}
			if err := validateProfileNames(layer.Profiles); err != nil {
				return Config{}, fmt.Errorf("config %q: %w", globalPath, err)
			}
			resolveGlobalPaths(&layer, cfg.configRoot)
			merge(&cfg, layer)
		} else if !errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("inspect config %q: %w", globalPath, err)
		}
	} else {
		// Keep state/credential paths, but exclude every user-discovered input.
		cfg.MCPConfig = ""
		cfg.ExtensionDirs = removePath(cfg.ExtensionDirs, filepath.Join(cfg.configRoot, "extensions"))
		cfg.SkillDirs = removePath(cfg.SkillDirs, filepath.Join(cfg.configRoot, "skills"))
		cfg.PromptDirs = removePath(cfg.PromptDirs, filepath.Join(cfg.configRoot, "prompts"))
		cfg.ThemeDirs = removePath(cfg.ThemeDirs, filepath.Join(cfg.configRoot, "themes"))
		cfg.AgentSkillDirs = removePath(cfg.AgentSkillDirs, filepath.Join(home, ".agents", "skills"))
		cfg.AgentCommandDirs = removePath(cfg.AgentCommandDirs, filepath.Join(home, ".agents", "commands"))
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
				if strings.TrimSpace(layer.SelectedProfile) != "" {
					return Config{}, fmt.Errorf("project config %q cannot select profile %q; use --profile at startup", projectPath, layer.SelectedProfile)
				}
				if len(layer.Profiles) != 0 {
					return Config{}, fmt.Errorf("project config %q cannot define profiles; profiles are global-only", projectPath)
				}
				if len(layer.Roles) != 0 {
					return Config{}, fmt.Errorf("project config %q cannot define roles; roles are global-only", projectPath)
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
		if normalizeRoutingProvider(value) != normalizeRoutingProvider(cfg.Provider) {
			cfg.BaseURL = ""
			cfg.Model = defaultModelForProvider(value)
		}
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

func removePath(paths []string, excluded string) []string {
	out := paths[:0]
	for _, path := range paths {
		if filepath.Clean(path) != filepath.Clean(excluded) {
			out = append(out, path)
		}
	}
	return out
}

func read(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := validateRawProfileNames(data); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := validateRoleNames(cfg.Roles); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	return cfg, nil
}

func mergeProject(dst *Config, src Config) {
	// These values choose remote endpoints, executable-update policy, or
	// credential/session/cache files and therefore remain global-only even
	// after the workspace is trusted.
	providerChanged := src.Provider != "" && !strings.EqualFold(strings.TrimSpace(src.Provider), strings.TrimSpace(dst.Provider))
	src.Profiles = nil
	src.Roles = nil
	src.SelectedProfile = ""
	src.CredentialRef = ""
	src.BaseURL = ""
	src.AuthFile = ""
	src.MCPAuthFile = ""
	src.SessionDir = ""
	src.ModelCache = ""
	src.AutoUpdate = nil
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
		if normalizeRoutingProvider(src.Provider) != normalizeRoutingProvider(dst.Provider) {
			dst.BaseURL = ""
			dst.Model = defaultModelForProvider(src.Provider)
		}
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
	if len(src.Profiles) != 0 {
		if dst.Profiles == nil {
			dst.Profiles = make(map[string]ProfileConfig, len(src.Profiles))
		}
		for key, profile := range src.Profiles {
			dst.Profiles[strings.ToLower(strings.TrimSpace(key))] = profile
		}
	}
	if len(src.Roles) != 0 {
		if dst.Roles == nil {
			dst.Roles = make(map[string]RoleConfig, len(src.Roles))
		}
		for key, role := range src.Roles {
			dst.Roles[CanonicalRoleName(key)] = role
		}
	}
	if src.MouseCapture != nil {
		value := *src.MouseCapture
		dst.MouseCapture = &value
	}
	if src.ContextWindow > 0 {
		dst.ContextWindow = src.ContextWindow
	}
	if src.DisablePricingRefresh != nil {
		value := *src.DisablePricingRefresh
		dst.DisablePricingRefresh = &value
	}
	if src.ModelRefreshHours > 0 {
		dst.ModelRefreshHours = src.ModelRefreshHours
	}
	if src.AutoUpdate != nil {
		value := *src.AutoUpdate
		dst.AutoUpdate = &value
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

// ResolveProfile applies a named global profile after environment settings.
// Explicit CLI overrides should be applied after this call and then checked
// with RoutingSnapshot.ValidateProvider.
func ResolveProfile(cfg *Config, name string) (RoutingSnapshot, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return RoutingSnapshot{}, nil
	}
	profile, ok := cfg.Profiles[name]
	if !ok {
		names := make([]string, 0, len(cfg.Profiles))
		for candidate := range cfg.Profiles {
			names = append(names, candidate)
		}
		sort.Strings(names)
		if len(names) == 0 {
			return RoutingSnapshot{}, fmt.Errorf("unknown profile %q; no profiles are defined in the user config", name)
		}
		return RoutingSnapshot{}, fmt.Errorf("unknown profile %q; available profiles: %s", name, strings.Join(names, ", "))
	}
	profile.Provider = normalizeRoutingProvider(profile.Provider)
	profile.Model = strings.TrimSpace(profile.Model)
	profile.ThinkingLevel = strings.ToLower(strings.TrimSpace(profile.ThinkingLevel))
	profile.CredentialRef = strings.TrimSpace(profile.CredentialRef)
	if profile.Provider == "" {
		return RoutingSnapshot{}, fmt.Errorf("profile %q: provider is required", name)
	}
	if !supportedRoutingProvider(profile.Provider) {
		return RoutingSnapshot{}, fmt.Errorf("profile %q: unsupported provider %q (expected openai-codex, anthropic-claude-code, openrouter, anthropic, or openai)", name, profile.Provider)
	}
	if profile.CredentialRef != "" && !validCredentialReference(profile.Provider, profile.CredentialRef) {
		return RoutingSnapshot{}, fmt.Errorf("profile %q: credential_ref %q must be %q or use the provider-prefixed form %q", name, profile.CredentialRef, profile.Provider, profile.Provider+":NAME")
	}
	if profile.Model == "" {
		return RoutingSnapshot{}, fmt.Errorf("profile %q: model is required", name)
	}
	if profile.ThinkingLevel == "" {
		profile.ThinkingLevel = cfg.ThinkingLevel
	}
	if !validProfileThinking(profile.ThinkingLevel) {
		return RoutingSnapshot{}, fmt.Errorf("profile %q: invalid thinking level %q", name, profile.ThinkingLevel)
	}
	allowed := normalizeProviders(profile.AllowedProviders)
	if len(allowed) == 0 {
		allowed = []string{profile.Provider}
	}
	for _, provider := range allowed {
		if !supportedRoutingProvider(provider) {
			return RoutingSnapshot{}, fmt.Errorf("profile %q: unsupported allowed provider %q", name, provider)
		}
	}
	roleModels := normalizeRoleModels(profile.RoleModels)
	seenRoleModels := map[string]string{}
	for rawRole, rawModel := range profile.RoleModels {
		canonicalRole := CanonicalRoleName(rawRole)
		if canonicalRole == "" || strings.TrimSpace(rawModel) == "" {
			return RoutingSnapshot{}, fmt.Errorf("profile %q role_models names and models must not be empty", name)
		}
		if prior, exists := seenRoleModels[canonicalRole]; exists {
			return RoutingSnapshot{}, fmt.Errorf("profile %q role_models names %q and %q resolve to the same role", name, prior, rawRole)
		}
		seenRoleModels[canonicalRole] = rawRole
	}
	for roleName, modelName := range roleModels {
		if _, err := ResolveRole(*cfg, nil, roleName); err != nil {
			return RoutingSnapshot{}, fmt.Errorf("profile %q role_models: %w", name, err)
		}
		provider := profile.Provider
		if prefix, _, found := strings.Cut(modelName, "/"); found && supportedRoutingProvider(prefix) {
			provider = normalizeRoutingProvider(prefix)
		}
		if !containsProvider(allowed, provider) {
			return RoutingSnapshot{}, fmt.Errorf("profile %q role %q selects provider %q, which is outside allowed_providers", name, roleName, provider)
		}
	}
	snapshot := RoutingSnapshot{Profile: name, Provider: profile.Provider, Model: profile.Model, ThinkingLevel: profile.ThinkingLevel, AllowedProviders: allowed, CredentialRef: profile.CredentialRef, RoleModels: roleModels}
	if err := snapshot.ValidateProvider(profile.Provider); err != nil {
		return RoutingSnapshot{}, fmt.Errorf("profile %q: %w", name, err)
	}
	canonical, _ := json.Marshal(snapshot)
	sum := sha256.Sum256(canonical)
	snapshot.DefinitionHash = hex.EncodeToString(sum[:])
	if normalizeRoutingProvider(cfg.Provider) != profile.Provider {
		cfg.BaseURL = ""
	}
	cfg.Provider, cfg.Model, cfg.ThinkingLevel, cfg.CredentialRef = profile.Provider, profile.Model, profile.ThinkingLevel, profile.CredentialRef
	return snapshot, nil
}

// ApplyRoutingSnapshot restores immutable non-secret routing inherited by a
// child process. It intentionally ignores profile definitions read by the child.
func ApplyRoutingSnapshot(cfg *Config, snapshot RoutingSnapshot) error {
	if strings.TrimSpace(snapshot.Profile) == "" || strings.TrimSpace(snapshot.DefinitionHash) == "" {
		return errors.New("routing snapshot requires profile and definition_hash")
	}
	if err := snapshot.ValidateProvider(snapshot.Provider); err != nil {
		return err
	}
	provider := normalizeRoutingProvider(snapshot.Provider)
	if !supportedRoutingProvider(provider) {
		return fmt.Errorf("routing snapshot has unsupported provider %q", snapshot.Provider)
	}
	for _, allowed := range snapshot.AllowedProviders {
		if !supportedRoutingProvider(allowed) {
			return fmt.Errorf("routing snapshot has unsupported allowed provider %q", allowed)
		}
	}
	if snapshot.CredentialRef != "" && !validCredentialReference(provider, snapshot.CredentialRef) {
		return fmt.Errorf("routing snapshot credential_ref %q does not belong to provider %q", snapshot.CredentialRef, provider)
	}
	if strings.TrimSpace(snapshot.Model) == "" {
		return errors.New("routing snapshot model is required")
	}
	if !validProfileThinking(snapshot.ThinkingLevel) {
		return fmt.Errorf("routing snapshot has invalid thinking level %q", snapshot.ThinkingLevel)
	}
	if normalizeRoutingProvider(cfg.Provider) != normalizeRoutingProvider(snapshot.Provider) {
		cfg.BaseURL = ""
	}
	cfg.Provider = normalizeRoutingProvider(snapshot.Provider)
	cfg.Model = strings.TrimSpace(snapshot.Model)
	cfg.ThinkingLevel = strings.ToLower(strings.TrimSpace(snapshot.ThinkingLevel))
	cfg.CredentialRef = strings.TrimSpace(snapshot.CredentialRef)
	return nil
}

func (s RoutingSnapshot) ValidateProvider(provider string) error {
	provider = normalizeRoutingProvider(provider)
	for _, allowed := range s.AllowedProviders {
		if normalizeRoutingProvider(allowed) == provider {
			return nil
		}
	}
	return fmt.Errorf("provider %q is not allowed by profile %q (allowed: %s)", provider, s.Profile, strings.Join(s.AllowedProviders, ", "))
}

func containsProvider(values []string, wanted string) bool {
	wanted = normalizeRoutingProvider(wanted)
	for _, value := range values {
		if normalizeRoutingProvider(value) == wanted {
			return true
		}
	}
	return false
}

func normalizeProviders(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = normalizeRoutingProvider(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func normalizeRoutingProvider(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
func validProfileThinking(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func defaultModelForProvider(provider string) string {
	switch normalizeRoutingProvider(provider) {
	case "openai-codex":
		return "gpt-5.6-terra"
	case "openrouter":
		return "anthropic/claude-sonnet-4.5"
	case "anthropic", "anthropic-claude-code":
		return "claude-sonnet-4-5"
	default:
		return "gpt-5"
	}
}

func validateRoleNames(roles map[string]RoleConfig) error {
	seen := make(map[string]string, len(roles))
	for name := range roles {
		canonical := CanonicalRoleName(name)
		if canonical == "" {
			return errors.New("role names must not be empty")
		}
		if prior, ok := seen[canonical]; ok {
			return fmt.Errorf("role names %q and %q resolve to the same canonical role", prior, name)
		}
		seen[canonical] = name
	}
	return nil
}

func validateProfileNames(profiles map[string]ProfileConfig) error {
	seen := make(map[string]string, len(profiles))
	for name := range profiles {
		normalized := strings.ToLower(strings.TrimSpace(name))
		if normalized == "" {
			return errors.New("profile names must not be empty")
		}
		if prior, ok := seen[normalized]; ok {
			return fmt.Errorf("profile names %q and %q differ only by case or surrounding whitespace", prior, name)
		}
		seen[normalized] = name
	}
	return nil
}

func supportedRoutingProvider(provider string) bool {
	switch normalizeRoutingProvider(provider) {
	case "openai-codex", "anthropic-claude-code", "openrouter", "anthropic", "openai":
		return true
	default:
		return false
	}
}

func validateRawProfileNames(data []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	start, err := decoder.Token()
	if err != nil {
		return err
	}
	if start != json.Delim('{') {
		return errors.New("top-level config must be an object")
	}
	seen := map[string]string{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("config object key must be a string")
		}
		if !strings.EqualFold(key, "profiles") {
			var discard json.RawMessage
			if err := decoder.Decode(&discard); err != nil {
				return err
			}
			continue
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if token == nil {
			continue
		}
		if token != json.Delim('{') {
			return errors.New("profiles must be an object")
		}
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return errors.New("profile name must be a string")
			}
			normalized := strings.ToLower(strings.TrimSpace(name))
			if normalized == "" {
				return errors.New("profile names must not be empty")
			}
			if prior, ok := seen[normalized]; ok {
				return fmt.Errorf("profile names %q and %q differ only by case or surrounding whitespace, or are duplicated", prior, name)
			}
			seen[normalized] = name
			var discard json.RawMessage
			if err := decoder.Decode(&discard); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func validCredentialReference(provider, reference string) bool {
	provider = normalizeRoutingProvider(provider)
	reference = strings.TrimSpace(reference)
	return reference == provider || strings.HasPrefix(reference, provider+":")
}
