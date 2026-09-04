package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/config"
	"github.com/trobrock/notch/internal/credentials"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/extpkg"
	"github.com/trobrock/notch/internal/luaext"
	"github.com/trobrock/notch/internal/mcp"
	"github.com/trobrock/notch/internal/mcpoauth"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/modelregistry"
	"github.com/trobrock/notch/internal/oauth"
	"github.com/trobrock/notch/internal/officialext"
	"github.com/trobrock/notch/internal/provider/anthropic"
	"github.com/trobrock/notch/internal/provider/codex"
	"github.com/trobrock/notch/internal/provider/openai"
	"github.com/trobrock/notch/internal/provider/openrouter"
	"github.com/trobrock/notch/internal/providerauth"
	"github.com/trobrock/notch/internal/resources"
	notchrpc "github.com/trobrock/notch/internal/rpc"
	"github.com/trobrock/notch/internal/session"
	"github.com/trobrock/notch/internal/tools"
	"github.com/trobrock/notch/internal/tui"
	"github.com/trobrock/notch/internal/ui"
	"github.com/trobrock/notch/internal/upgrade"
	"github.com/trobrock/notch/internal/workspace"
	"golang.org/x/term"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

const notchUsage = `Usage: notch [OPTIONS] [PROMPT]
       notch COMMAND [ARGS]

Commands:
  version              show detailed version and build information
  upgrade              check for or install a Notch release
  models               list available provider models
  login PROVIDER       authenticate with a provider
  logout PROVIDER      remove a stored provider credential
  auth                 inspect or import provider credentials
  mcp                   manage MCP authentication and status
  extensions            manage extension packages

Run 'notch COMMAND --help' for command-specific help.`

type options struct {
	provider, modelName, thinking, prompt, systemPrompt, systemPromptFile, mcpConfig, resumeSession, mode, toolAllow, toolExclude string
	settingSources                                                                                                                string
	maxTurns                                                                                                                      int
	maxCostUSD                                                                                                                    float64
	idleTimeout                                                                                                                   time.Duration
	printMode, planMode, continueSession, resumeSpecified, noSession, jsonOutput, noTUI, init, showVersion, rpcMode               bool
	noTools, noBuiltinTools, noExtensions, noResources                                                                            bool
	safe, trustWorkspace                                                                                                          bool
}

type optionalStringFlag struct {
	value     *string
	specified *bool
}

func (f *optionalStringFlag) String() string {
	if f == nil || f.value == nil {
		return ""
	}
	return *f.value
}
func (f *optionalStringFlag) Set(value string) error {
	*f.value = value
	*f.specified = true
	return nil
}

func normalizeResumeArgs(args []string) []string {
	result := append([]string(nil), args...)
	for i, arg := range result {
		if (arg == "--resume" || arg == "-r") && (i+1 == len(result) || strings.HasPrefix(result[i+1], "-")) {
			result[i] += "="
		}
	}
	return result
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "notch:", err)
		os.Exit(processExitCode(err))
	}
}

func processExitCode(err error) int {
	if errors.Is(err, agent.ErrMaxTurns) || errors.Is(err, agent.ErrMaxCost) {
		return 2
	}
	return 1
}

func newRootFlagSet(opts *options) *flag.FlagSet {
	flags := flag.NewFlagSet("notch", flag.ContinueOnError)
	resume := optionalStringFlag{value: &opts.resumeSession, specified: &opts.resumeSpecified}
	flags.StringVar(&opts.provider, "provider", "", "provider: openai-codex, anthropic-claude-code, openrouter, anthropic, or openai")
	flags.StringVar(&opts.modelName, "model", "", "model ID")
	flags.StringVar(&opts.modelName, "m", "", "model ID (shorthand)")
	flags.StringVar(&opts.thinking, "thinking", "", "reasoning effort: off, minimal, low, medium, high, or xhigh")
	flags.IntVar(&opts.maxTurns, "max-turns", 0, "maximum model turns per run (0 is unlimited)")
	flags.Float64Var(&opts.maxCostUSD, "max-cost-usd", 0, "maximum cumulative run cost in USD (0 is unlimited)")
	flags.DurationVar(&opts.idleTimeout, "idle-timeout", 0, "cancel a run after no provider or tool events for this duration")
	flags.StringVar(&opts.settingSources, "setting-sources", "user,project", "configuration/resource sources: user, project, user,project, or none")
	flags.BoolVar(&opts.printMode, "print", false, "non-interactive mode: process prompt and exit")
	flags.BoolVar(&opts.printMode, "p", false, "non-interactive mode (shorthand)")
	flags.BoolVar(&opts.planMode, "plan", false, "start with read-only plan mode enabled")
	flags.StringVar(&opts.systemPrompt, "system-prompt", "", "override the configured system prompt")
	flags.StringVar(&opts.systemPromptFile, "system-prompt-file", "", "read the system prompt override from a file")
	flags.StringVar(&opts.mcpConfig, "mcp-config", "", "path to MCP JSON config")
	flags.BoolVar(&opts.continueSession, "continue", false, "continue the latest session for this working directory")
	flags.Var(&resume, "resume", "select a recent session; pass an ID, prefix, filename, or path to resume directly")
	flags.Var(&resume, "r", "resume a session (shorthand)")
	flags.BoolVar(&opts.noSession, "no-session", false, "do not save a session")
	flags.BoolVar(&opts.jsonOutput, "json", false, "emit JSONL events")
	flags.BoolVar(&opts.noTUI, "no-tui", false, "use the line-oriented interface")
	flags.StringVar(&opts.mode, "mode", "", "run mode: rpc")
	flags.BoolVar(&opts.rpcMode, "rpc", false, "run Pi-compatible JSONL RPC mode")
	flags.StringVar(&opts.toolAllow, "tools", "", "strict comma-separated tool allowlist")
	flags.StringVar(&opts.toolAllow, "t", "", "tool allowlist (shorthand)")
	flags.StringVar(&opts.toolExclude, "exclude-tools", "", "comma-separated tools to disable")
	flags.StringVar(&opts.toolExclude, "xt", "", "excluded tools (shorthand)")
	flags.BoolVar(&opts.noTools, "no-tools", false, "disable all model tools")
	flags.BoolVar(&opts.noTools, "nt", false, "disable all tools (shorthand)")
	flags.BoolVar(&opts.noBuiltinTools, "no-builtin-tools", false, "disable built-in tools")
	flags.BoolVar(&opts.noBuiltinTools, "nbt", false, "disable built-in tools (shorthand)")
	flags.BoolVar(&opts.noExtensions, "no-extensions", false, "disable official and configured extensions")
	flags.BoolVar(&opts.noResources, "no-resources", false, "disable skills and prompt templates")
	flags.BoolVar(&opts.safe, "safe", false, "skip project configuration, extensions, and resources")
	flags.BoolVar(&opts.trustWorkspace, "trust-workspace", false, "persist trust in this workspace")
	flags.BoolVar(&opts.init, "init", false, "create the XDG config directory and a starter config")
	flags.BoolVar(&opts.showVersion, "version", false, "print version")
	return flags
}

func selectRunMode(opts options, interactive bool) (fullscreen, oneShot bool) {
	fullscreen = !opts.rpcMode && opts.mode != "rpc" && !opts.printMode && !opts.jsonOutput && !opts.noTUI && interactive
	oneShot = opts.prompt != "" && !fullscreen
	return fullscreen, oneShot
}

func run(args []string) error {
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		fmt.Println(notchUsage)
		fmt.Println("\nOptions:")
		flags := newRootFlagSet(&options{})
		flags.SetOutput(os.Stdout)
		flags.PrintDefaults()
		return nil
	}
	if len(args) > 0 && args[0] == "version" {
		return runVersion(args[1:])
	}
	if len(args) > 0 && args[0] == "upgrade" {
		return runUpgrade(args[1:])
	}
	if len(args) > 0 && (args[0] == "login" || args[0] == "logout" || args[0] == "auth") {
		return runAuth(args)
	}
	if len(args) > 0 && args[0] == "mcp" {
		return runMCP(args[1:])
	}
	if len(args) > 0 && args[0] == "models" {
		return runListModels(args[1:])
	}
	if len(args) > 0 && (args[0] == "extension" || args[0] == "extensions") {
		return runExtensions(args[1:])
	}
	var opts options
	flags := newRootFlagSet(&opts)
	flags.SetOutput(os.Stderr)
	if err := flags.Parse(normalizeResumeArgs(args)); err != nil {
		return err
	}
	if opts.showVersion {
		fmt.Println("notch", currentBuildInfo().Version)
		return nil
	}
	if opts.mode != "" && opts.mode != "rpc" {
		return fmt.Errorf("unsupported mode %q (available: rpc)", opts.mode)
	}
	rpcMode := opts.rpcMode || opts.mode == "rpc"
	if rpcMode && (opts.printMode || flags.NArg() != 0 || opts.jsonOutput) {
		return errors.New("RPC mode cannot be combined with --print, --json, or prompt arguments")
	}
	if opts.safe && opts.trustWorkspace {
		return errors.New("--safe and --trust-workspace cannot be combined")
	}
	if opts.maxTurns < 0 {
		return errors.New("--max-turns must be non-negative")
	}
	if opts.maxCostUSD < 0 || math.IsNaN(opts.maxCostUSD) || math.IsInf(opts.maxCostUSD, 0) {
		return errors.New("--max-cost-usd must be a finite non-negative number")
	}
	if opts.idleTimeout < 0 {
		return errors.New("--idle-timeout must be non-negative")
	}
	includeUser, includeProject, sourceErr := parseSettingSources(opts.settingSources)
	if sourceErr != nil {
		return sourceErr
	}
	if opts.safe && opts.settingSources != "user,project" {
		return errors.New("--safe and --setting-sources cannot be combined")
	}
	if opts.noExtensions && opts.planMode {
		return errors.New("--plan cannot be combined with --no-extensions")
	}
	if opts.planMode && (opts.noTools || opts.noBuiltinTools || opts.toolAllow != "" || opts.toolExclude != "") {
		return errors.New("--plan cannot be combined with tool restriction flags")
	}
	if opts.noTools && (opts.toolAllow != "" || opts.toolExclude != "") {
		return errors.New("--no-tools cannot be combined with --tools or --exclude-tools")
	}
	if opts.continueSession && opts.resumeSpecified {
		return errors.New("--continue and --resume cannot be used together")
	}
	if opts.noSession && (opts.continueSession || opts.resumeSpecified) {
		return errors.New("--no-session cannot be combined with --continue or --resume")
	}
	if flags.NArg() != 0 {
		opts.prompt = strings.Join(flags.Args(), " ")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home directory: %w", err)
	}
	workspaceRoot := cwd
	workspaceTrustKey := cwd
	workspaceTrusted := false
	gitBranch := ""
	var cfg config.Config
	switch {
	case opts.init:
		// Initialization must never inspect, prompt for, trust, or load project
		// inputs. cwd is retained only for the project paths printed below.
		cfg, err = config.LoadGlobal(home)
	case opts.safe:
		// Safe mode is also an emergency bypass for malformed or hostile Git
		// metadata, so it must not perform workspace discovery.
		cfg, err = config.LoadWorkspace(home, cwd, false)
	case !includeProject:
		// Explicit user-only/none isolation must not inspect or prompt for project inputs.
		cfg, err = config.LoadWorkspaceSources(home, cwd, includeUser, false)
	default:
		workspaceInfo, resolveErr := workspace.Resolve(cwd)
		if resolveErr != nil {
			return resolveErr
		}
		workspaceRoot = workspaceInfo.Root
		workspaceTrustKey = workspaceInfo.TrustKey
		gitBranch = workspaceInfo.Branch
		workspaceTrusted, err = resolveWorkspaceTrust(home, workspaceRoot, workspaceTrustKey, opts, os.Stdin, os.Stderr, terminalsInteractive(os.Stdin, os.Stdout))
		if err == nil {
			cfg, err = config.LoadWorkspaceSources(home, workspaceRoot, includeUser, workspaceTrusted && includeProject)
		}
	}
	if err != nil {
		return err
	}
	if opts.provider != "" {
		changed := normalizeProvider(opts.provider) != normalizeProvider(cfg.Provider)
		if changed {
			cfg.BaseURL = ""
			if opts.modelName == "" {
				cfg.Model = defaultModelFor(opts.provider)
			}
		}
		cfg.Provider = opts.provider
	}
	if opts.modelName != "" {
		cfg.Model = opts.modelName
	}
	if opts.thinking != "" {
		cfg.ThinkingLevel = strings.ToLower(strings.TrimSpace(opts.thinking))
		if !validThinkingLevel(cfg.ThinkingLevel) {
			return fmt.Errorf("invalid thinking level %q (expected off, minimal, low, medium, high, or xhigh)", opts.thinking)
		}
	}
	cfg.CacheRetention = strings.ToLower(strings.TrimSpace(cfg.CacheRetention))
	if cfg.CacheRetention != "none" && cfg.CacheRetention != "short" && cfg.CacheRetention != "long" {
		return fmt.Errorf("invalid cache_retention %q (expected none, short, or long)", cfg.CacheRetention)
	}
	if opts.init {
		return initialize(home, workspaceRoot, cfg)
	}
	if opts.mcpConfig != "" {
		cfg.MCPConfig = opts.mcpConfig
	}
	if opts.systemPrompt != "" && opts.systemPromptFile != "" {
		return errors.New("--system-prompt and --system-prompt-file cannot be combined")
	}
	if opts.systemPromptFile != "" {
		data, readErr := os.ReadFile(opts.systemPromptFile)
		if readErr != nil {
			return fmt.Errorf("read system prompt file: %w", readErr)
		}
		cfg.SystemPrompt = string(data)
	} else if opts.systemPrompt != "" {
		cfg.SystemPrompt = opts.systemPrompt
	}
	workspaceInstructions := ""
	if workspaceTrusted {
		workspaceInstructions, err = workspace.Instructions(workspaceRoot)
		if err != nil {
			return err
		}
	}
	if opts.noExtensions {
		cfg.ExtensionDirs = nil
	}
	var packageDirs []string
	if !opts.noExtensions && includeUser {
		packageDirs, err = func() ([]string, error) {
			dataRoot, rootErr := config.DataDir(home)
			if rootErr != nil {
				return nil, rootErr
			}
			return extpkg.DiscoveryDirs(dataRoot)
		}()
		if err != nil {
			return fmt.Errorf("load installed extension packages: %w", err)
		}
	}
	cfg.ExtensionDirs = append(cfg.ExtensionDirs, packageDirs...)
	if err := cfg.EnsureGlobalDirs(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	interactiveTerminal := terminalsInteractive(os.Stdin, os.Stdout)
	type startupNotice struct{ message, level string }
	var startupNotices []startupNotice
	if interactiveTerminal && !rpcMode && (cfg.AutoUpdate == nil || *cfg.AutoUpdate) {
		dataRoot, rootErr := config.DataDir(home)
		if rootErr != nil {
			startupNotices = append(startupNotices, startupNotice{rootErr.Error(), "warning"})
		} else {
			updateCtx, cancelUpdate := context.WithTimeout(ctx, 30*time.Second)
			result, checked, updateErr := upgrade.Automatic(updateCtx, upgrade.AutomaticOptions{
				Upgrade:   upgrade.Options{CurrentVersion: currentBuildInfo().Version},
				StatePath: filepath.Join(dataRoot, "auto-update.json"),
			})
			cancelUpdate()
			switch {
			case updateErr != nil && !errors.Is(updateErr, context.Canceled):
				startupNotices = append(startupNotices, startupNotice{"Automatic update failed: " + updateErr.Error(), "warning"})
			case checked && result.Updated:
				restarted, restartErr := restartSelf()
				if restartErr != nil {
					startupNotices = append(startupNotices, startupNotice{"Updated Notch, but could not restart automatically: " + restartErr.Error(), "warning"})
				} else if restarted {
					return nil
				}
			}
		}
	}
	terminal := ui.DefaultTerminal(cwd)
	themeCatalog, themeWarnings := tui.LoadThemeCatalog(cfg.ThemeDirs...)
	selectedTheme, selectedThemeName, ok := themeCatalog.Lookup(cfg.Theme)
	if !ok {
		for _, warning := range themeWarnings {
			terminal.Notify(warning.Error(), "warning")
		}
		return fmt.Errorf("unknown theme %q (available: %s)", cfg.Theme, strings.Join(themeCatalog.Names(), ", "))
	}
	cfg.Theme = selectedThemeName
	if opts.resumeSpecified && opts.resumeSession == "" {
		if !interactiveTerminal || rpcMode {
			return errors.New("--resume without a session ID requires an interactive terminal")
		}
		infos, listErr := session.List(cfg.SessionDir)
		if listErr != nil {
			return listErr
		}
		recent := make([]session.Info, 0, 5)
		for _, info := range infos {
			if info.MessageCount > 0 && filepath.Clean(info.Header.CWD) == filepath.Clean(cwd) {
				recent = append(recent, info)
				if len(recent) == 5 {
					break
				}
			}
		}
		if runtime.GOOS == "windows" {
			choices := make([]string, len(recent))
			for i, info := range recent {
				choices[i] = fmt.Sprintf("%s — %s", info.ModifiedAt.Local().Format("Jan 02 15:04"), info.Preview)
			}
			choice, selectErr := terminal.Select(ctx, "Resume session", choices)
			if selectErr != nil {
				return selectErr
			}
			for i, label := range choices {
				if label == choice {
					opts.resumeSession = recent[i].Path
					break
				}
			}
		} else {
			selected, selectErr := tui.SelectRecentSession(ctx, os.Stdin, os.Stdout, recent, selectedTheme)
			if errors.Is(selectErr, context.Canceled) {
				return nil
			}
			if selectErr != nil {
				return selectErr
			}
			opts.resumeSession = selected.Path
		}
	}
	useFullscreen, _ := selectRunMode(opts, interactiveTerminal)
	sessionDir := cfg.SessionDir
	if opts.noSession {
		sessionDir = ""
	}
	var fullscreen *tui.App
	var rpcServer *notchrpc.Server
	var extensionHost extension.Host = terminal
	if rpcMode {
		rpcServer = notchrpc.New(os.Stdin, os.Stdout, cwd)
		extensionHost = rpcServer
	} else if useFullscreen {
		fullscreen = tui.NewApp(tui.AppConfig{
			CWD: cwd, Provider: normalizeProvider(cfg.Provider), Model: cfg.Model, SessionDir: sessionDir,
			Theme: selectedTheme, ThemeName: cfg.Theme, Themes: themeCatalog, ThinkingLevel: cfg.ThinkingLevel,
			Presets:       tuiPresets(cfg.Presets),
			InitialPrompt: opts.prompt, MouseCapture: cfg.MouseCapture,
			GitBranch: gitBranch, In: os.Stdin, Out: os.Stdout,
		})
		extensionHost = fullscreen
		for _, warning := range themeWarnings {
			fullscreen.Notify(warning.Error(), "warning")
		}
	} else {
		for _, warning := range themeWarnings {
			terminal.Notify(warning.Error(), "warning")
		}
	}
	for _, notice := range startupNotices {
		extensionHost.Notify(notice.message, notice.level)
	}
	registry := extension.NewRegistry()
	if !opts.noTools {
		if !opts.noBuiltinTools {
			if err := tools.RegisterBuiltins(registry, cwd); err != nil {
				return err
			}
		}
		if !opts.noExtensions {
			if err := officialext.RegisterWithSettingSources(registry, extensionHost, opts.settingSources); err != nil {
				return err
			}
		}
	}

	var plugins []*extension.Plugin
	var warnings []error
	if !opts.noExtensions {
		plugins, warnings = extension.DiscoverAndLoad(ctx, cfg.ExtensionDirs, registry, extensionHost)
	}
	defer func() {
		for i := len(plugins) - 1; i >= 0; i-- {
			_ = plugins[i].Close()
		}
	}()
	for _, warning := range warnings {
		extensionHost.Notify(warning.Error(), "warning")
	}

	luaManager := luaext.New(registry, extensionHost)
	if !opts.noExtensions {
		if err := luaManager.LoadDirs(cfg.ExtensionDirs...); err != nil {
			extensionHost.Notify(err.Error(), "warning")
		}
	}
	defer luaManager.Close()

	var mcpRuntime *mcpRuntime
	mcpPolicyReady := false
	if cfg.MCPConfig != "" && !opts.noTools {
		if _, statErr := os.Stat(cfg.MCPConfig); statErr == nil {
			mcpCfg, loadErr := mcp.LoadConfig(cfg.MCPConfig)
			if loadErr != nil {
				extensionHost.Notify(loadErr.Error(), "warning")
			} else {
				oauthStore := mcpoauth.NewStore(cfg.MCPAuthFile)
				authorizer := &mcpoauth.Authorizer{Store: oauthStore, Client: mcpoauth.NewClient()}
				mcpRuntime = newMCPRuntime(mcpCfg, registry, oauthStore, authorizer, func() {
					if mcpPolicyReady {
						reapplyToolPolicy(registry, opts)
					}
				})
				if fullscreen != nil {
					if err := registry.RegisterCommand(mcpRuntime.command(mcpNoticeWriter{host: extensionHost})); err != nil {
						return err
					}
				}
				if err = mcpRuntime.Connect(ctx); err != nil {
					extensionHost.Notify(err.Error(), "warning")
					if hint := mcpLoginHint(err, mcpCfg); fullscreen != nil && hint != "" {
						extensionHost.Notify(hint, "notice")
					}
				}
			}
		}
	}
	if mcpRuntime != nil {
		defer mcpRuntime.Close()
	}

	var catalog *resources.Catalog
	if opts.noResources {
		catalog = &resources.Catalog{Skills: map[string]resources.Skill{}, Templates: map[string]resources.Template{}}
	} else {
		catalog, err = resources.LoadBundled(cfg.SkillDiscoveryDirs(), cfg.PromptDiscoveryDirs())
		if err != nil {
			extensionHost.Notify(err.Error(), "warning")
		}
	}
	skillToolAvailable := false
	if !opts.noTools {
		if !opts.noBuiltinTools {
			registered, registerErr := catalog.RegisterSkillTool(registry)
			if registerErr != nil {
				return registerErr
			}
			skillToolAvailable = registered
		}
		if _, exists := registry.Tool("skill"); exists {
			skillToolAvailable = true
		}
	}
	if err := applyToolPolicy(registry, opts); err != nil {
		return err
	}
	mcpPolicyReady = true
	if _, active := registry.Tool("skill"); !active {
		skillToolAvailable = false
	}
	terminal.SetRegistry(registry)

	systemPrompt := cfg.SystemPrompt
	if workspaceInstructions != "" {
		systemPrompt += "\n\n" + workspaceInstructions
	}
	if summary := catalog.SystemSummary(skillToolAvailable); summary != "" {
		systemPrompt += "\n\n" + summary
	}
	if cfg.ExploreModel != "" {
		systemPrompt += "\n\nThe configured default for explore_codebase is `" + cfg.ExploreModel + "`. Omit model to use it. If it is unavailable, call list_models for its provider and retry once with the closest listed model in the same family and capability tier; never guess an unlisted model ID."
	}

	credentialStore := credentials.New(cfg.AuthFile)
	baseModelConfig := cfg
	modelsRegistry := modelRegistryFor(cfg)
	provider, err := makeProvider(ctx, cfg, credentialStore)
	if err != nil {
		return err
	}
	if lister, ok := provider.(model.ModelLister); ok {
		providerName, scope := normalizeProvider(cfg.Provider), modelregistry.Scope(normalizeProvider(cfg.Provider), cfg.BaseURL)
		go func() {
			refreshCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, _ = modelsRegistry.List(refreshCtx, providerName, scope, false, lister.ListModels)
		}()
	}
	var store *session.Session
	if !opts.noSession {
		if opts.resumeSpecified {
			var path string
			path, err = session.Resolve(cfg.SessionDir, opts.resumeSession)
			if err == nil {
				store, err = session.Load(path)
			}
		} else if opts.continueSession {
			store, err = session.LatestForCWD(cfg.SessionDir, cwd)
		} else {
			store, err = session.New(cfg.SessionDir, cwd, cfg.Provider, cfg.Model)
		}
		if err != nil {
			return err
		}
		defer store.Close()
	}
	// Extensions receive their host before the agent is configured. Publish the
	// initial session before session_start so durable extension state can be
	// restored during that lifecycle hook; /new's factory is added later.
	terminal.SetSession(store)
	if rpcServer != nil {
		rpcServer.SetSession(store)
	}
	if fullscreen != nil {
		fullscreen.SetSessionFactory(store, nil)
		defer fullscreen.CloseSession()
	}
	compactionEnabled := true
	reserveTokens, keepRecentTokens := 16384, 20000
	if cfg.Compaction != nil {
		if cfg.Compaction.Enabled != nil {
			compactionEnabled = *cfg.Compaction.Enabled
		}
		if cfg.Compaction.ReserveTokens > 0 {
			reserveTokens = cfg.Compaction.ReserveTokens
		}
		if cfg.Compaction.KeepRecentTokens > 0 {
			keepRecentTokens = cfg.Compaction.KeepRecentTokens
		}
	}
	contextWindow := cfg.ContextWindow
	if contextWindow <= 0 {
		cachedModels, _ := modelsRegistry.Cached(normalizeProvider(cfg.Provider), modelregistry.Scope(normalizeProvider(cfg.Provider), cfg.BaseURL))
		for _, candidate := range cachedModels {
			if candidate.ID == cfg.Model && candidate.ContextWindow > 0 {
				contextWindow = candidate.ContextWindow
				break
			}
		}
	}
	if contextWindow <= 0 {
		contextWindow = contextWindowFor(cfg.Provider, cfg.Model)
	}
	runner, err := agent.New(agent.Config{
		Provider: provider, ProviderName: normalizeProvider(cfg.Provider), Registry: registry, Session: store, Model: cfg.Model,
		ExploreModel: cfg.ExploreModel, SystemPrompt: systemPrompt, MaxTokens: cfg.MaxTokens, ThinkingLevel: cfg.ThinkingLevel, CacheRetention: cfg.CacheRetention,
		Compaction: agent.CompactionConfig{Enabled: compactionEnabled, ContextWindow: contextWindow, ReserveTokens: reserveTokens, KeepRecentTokens: keepRecentTokens},
		MaxTurns:   opts.maxTurns, MaxCostUSD: opts.maxCostUSD,
		SessionInfo: benchmarkSessionInfo(cfg, cwd, workspaceRoot, workspaceTrusted && includeProject, registry, catalog),
		IdleTimeout: opts.idleTimeout,
	})
	if err != nil {
		return err
	}
	lifecycleEvent := map[string]any{
		"cwd": cwd, "provider": normalizeProvider(cfg.Provider), "model": cfg.Model,
		"thinking_level": cfg.ThinkingLevel, "mode": runMode(rpcMode, useFullscreen, opts),
		"resumed": opts.continueSession || opts.resumeSpecified,
	}
	if store != nil {
		lifecycleEvent["session_id"] = store.Header.ID
		lifecycleEvent["session_file"] = store.Path()
	}
	shutdownLifecycle, err := beginSessionLifecycle(ctx, registry, lifecycleEvent)
	if err != nil {
		return err
	}
	defer func() {
		reason := "exit"
		if ctx.Err() != nil {
			reason = "canceled"
		}
		if shutdownErr := shutdownLifecycle(reason); shutdownErr != nil {
			terminal.Notify(shutdownErr.Error(), "warning")
		}
	}()
	if rpcServer != nil {
		sessionFile, sessionID := "", ""
		if store != nil {
			sessionFile, sessionID = store.Path(), store.Header.ID
		}
		rpcServer.Configure(runner, registry, catalog, notchrpc.StateConfig{
			Provider: normalizeProvider(cfg.Provider), Model: cfg.Model, API: rpcAPIForProvider(cfg.Provider), BaseURL: cfg.BaseURL,
			ContextWindow: contextWindow, MaxTokens: cfg.MaxTokens, SessionFile: sessionFile, SessionID: sessionID,
			AutoCompactionEnabled: compactionEnabled,
		})
		if opts.planMode {
			if err := enableStartupPlanMode(ctx, registry); err != nil {
				return err
			}
		}
		return rpcServer.Run(ctx)
	}
	if fullscreen != nil {
		fullscreen.SetModelManager(
			func(listCtx context.Context, providerName string, force bool) ([]modelregistry.Entry, error) {
				return discoverModels(listCtx, modelsRegistry, baseModelConfig, credentialStore, providerName, force)
			},
			func(switchCtx context.Context, providerName, modelName string, discoveredWindow int) (int, error) {
				candidate := configForProvider(baseModelConfig, providerName)
				candidate.Model = modelName
				nextProvider, makeErr := makeProvider(switchCtx, candidate, credentialStore)
				if makeErr != nil {
					return 0, makeErr
				}
				window := discoveredWindow
				if baseModelConfig.ContextWindow > 0 {
					window = baseModelConfig.ContextWindow
				}
				if window <= 0 {
					window = contextWindowFor(providerName, modelName)
				}
				if switchErr := runner.QueueProviderSwitch(normalizeProvider(providerName), nextProvider, modelName, window); switchErr != nil {
					return 0, switchErr
				}
				cfg.Provider, cfg.Model = normalizeProvider(providerName), modelName
				return window, nil
			},
		)
		var sessionFactory func() (*session.Session, error)
		if !opts.noSession {
			sessionFactory = func() (*session.Session, error) { return session.New(cfg.SessionDir, cwd, cfg.Provider, cfg.Model) }
		}
		fullscreen.SetSessionFactory(store, sessionFactory)
		fullscreen.Configure(runner, registry, catalog)
		if opts.planMode {
			if err := enableStartupPlanMode(ctx, registry); err != nil {
				return err
			}
		}
		if err := fullscreen.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}

	if opts.planMode {
		if err := enableStartupPlanMode(ctx, registry); err != nil {
			return err
		}
	}
	emit := terminal.Render
	if opts.jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		emit = func(event agent.Event) { _ = encoder.Encode(event) }
	}
	if opts.printMode && opts.prompt == "" {
		data, readErr := io.ReadAll(os.Stdin)
		if readErr != nil {
			return fmt.Errorf("read prompt from stdin: %w", readErr)
		}
		opts.prompt = strings.TrimSpace(string(data))
		if opts.prompt == "" {
			return nil
		}
	}
	if opts.prompt != "" {
		prompt, expandErr := catalog.ExpandInput(opts.prompt)
		if expandErr != nil && strings.HasPrefix(strings.TrimSpace(opts.prompt), "/") {
			return expandErr
		}
		if expandErr != nil {
			prompt = opts.prompt
		}
		if err := runner.Prompt(ctx, prompt, emit); err != nil {
			return err
		}
		if !opts.jsonOutput {
			fmt.Println()
		}
		return nil
	}

	if !opts.jsonOutput {
		fmt.Fprintf(os.Stderr, "Notch %s · %s/%s · %d tools\n", currentBuildInfo().Version, cfg.Provider, cfg.Model, len(registry.Tools()))
		if store != nil {
			fmt.Fprintf(os.Stderr, "session: %s\n", store.Path())
		}
	}
	for {
		input, readErr := terminal.ReadPrompt("notch> ")
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if input == "" {
			continue
		}
		if handled, exit, commandErr := handleCommand(ctx, input, registry, catalog, terminal, runner); handled {
			if commandErr != nil {
				terminal.Notify(commandErr.Error(), "error")
			}
			if exit {
				return nil
			}
			continue
		}
		expanded, expandErr := catalog.ExpandInput(input)
		if expandErr != nil && strings.HasPrefix(input, "/") {
			terminal.Notify(expandErr.Error(), "error")
			continue
		}
		if expandErr != nil {
			expanded = input
		}
		if err := runner.Prompt(ctx, expanded, emit); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			terminal.Notify(err.Error(), "error")
		}
		if !opts.jsonOutput {
			fmt.Println()
		}
	}
}

func parseSettingSources(value string) (bool, bool, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "none" {
		return false, false, nil
	}
	var user, project bool
	for _, source := range strings.Split(value, ",") {
		switch strings.TrimSpace(source) {
		case "user":
			user = true
		case "project":
			project = true
		default:
			return false, false, fmt.Errorf("invalid --setting-sources %q (expected user, project, user,project, or none)", value)
		}
	}
	return user, project, nil
}

func benchmarkSessionInfo(cfg config.Config, cwd, workspaceRoot string, trusted bool, registry *extension.Registry, catalog *resources.Catalog) agent.SessionInfo {
	tools := make([]string, 0, len(registry.Tools()))
	for _, tool := range registry.Tools() {
		tools = append(tools, tool.Definition.Name)
	}
	skills := make([]string, 0, len(catalog.Skills))
	for name := range catalog.Skills {
		skills = append(skills, name)
	}
	sort.Strings(skills)
	files := make([]string, 0, 2)
	if trusted {
		for _, name := range []string{"AGENTS.md", "AGENTS.local.md"} {
			path := filepath.Join(workspaceRoot, name)
			if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
				files = append(files, path)
			}
		}
	}
	return agent.SessionInfo{NotchVersion: currentBuildInfo().Version, Provider: normalizeProvider(cfg.Provider), Model: cfg.Model,
		ThinkingLevel: cfg.ThinkingLevel, Tools: tools, CWD: cwd, WorkspaceTrusted: trusted,
		InstructionFiles: files, Skills: skills}
}

const sessionLifecycleTimeout = 10 * time.Second

func beginSessionLifecycle(parent context.Context, registry *extension.Registry, fields map[string]any) (func(string) error, error) {
	startCtx, cancelStart := context.WithTimeout(parent, sessionLifecycleTimeout)
	_, err := registry.RunHooks(startCtx, "session_start", cloneEvent(fields))
	cancelStart()
	if err != nil {
		return nil, err
	}
	return func(reason string) error {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), sessionLifecycleTimeout)
		defer cancelShutdown()
		event := cloneEvent(fields)
		event["reason"] = reason
		_, shutdownErr := registry.RunHooksBestEffort(shutdownCtx, "session_shutdown", event)
		return shutdownErr
	}, nil
}

func cloneEvent(event map[string]any) map[string]any {
	cloned := make(map[string]any, len(event))
	for key, value := range event {
		cloned[key] = value
	}
	return cloned
}

func runMode(rpcMode, fullscreen bool, opts options) string {
	switch {
	case rpcMode:
		return "rpc"
	case fullscreen:
		return "tui"
	case opts.jsonOutput:
		return "json"
	case opts.prompt != "":
		return "print"
	default:
		return "line"
	}
}

func modelRegistryFor(cfg config.Config) *modelregistry.Registry {
	ttl := time.Duration(cfg.ModelRefreshHours) * time.Hour
	return modelregistry.New(cfg.ModelCache, ttl)
}

func configForProvider(base config.Config, providerName string) config.Config {
	providerName = normalizeProvider(providerName)
	candidate := base
	if providerName != normalizeProvider(base.Provider) {
		candidate.BaseURL = ""
	}
	candidate.Provider = providerName
	candidate.Model = defaultModelFor(providerName)
	return candidate
}

func discoverModels(ctx context.Context, registry *modelregistry.Registry, base config.Config, store *credentials.Store, providerName string, force bool) ([]modelregistry.Entry, error) {
	candidate := configForProvider(base, providerName)
	instance, providerErr := makeProvider(ctx, candidate, store)
	if providerErr != nil && normalizeProvider(providerName) == "openrouter" {
		// OpenRouter's model catalog is public even when generation credentials
		// have not been configured yet.
		instance, providerErr = openrouter.New(openrouter.Config{BaseURL: candidate.BaseURL, AppName: "Notch"}), nil
	}
	var fetch modelregistry.Fetcher
	if providerErr == nil {
		if lister, ok := instance.(model.ModelLister); ok {
			fetch = lister.ListModels
		} else {
			providerErr = errors.New("provider does not support model listing")
		}
	}
	models, registryErr := registry.List(ctx, normalizeProvider(providerName), modelregistry.Scope(normalizeProvider(providerName), candidate.BaseURL), force, fetch)
	return models, errors.Join(providerErr, registryErr)
}

type buildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

func currentBuildInfo() buildInfo {
	result := buildInfo{
		Version: version, Commit: commit, BuildDate: buildDate,
		GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if (result.Version == "" || result.Version == "dev") && info.Main.Version != "" && info.Main.Version != "(devel)" {
			result.Version = info.Main.Version
		}
		modified, usedVCSCommit := false, false
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if result.Commit == "" || result.Commit == "unknown" {
					result.Commit, usedVCSCommit = setting.Value, true
				}
			case "vcs.time":
				if result.BuildDate == "" || result.BuildDate == "unknown" {
					result.BuildDate = setting.Value
				}
			case "vcs.modified":
				modified = setting.Value == "true"
			}
		}
		if modified && usedVCSCommit && result.Commit != "" && result.Commit != "unknown" && !strings.HasSuffix(result.Commit, "-dirty") {
			result.Commit += "-dirty"
		}
	}
	if result.Version == "" {
		result.Version = "dev"
	}
	if result.Commit == "" {
		result.Commit = "unknown"
	}
	if result.BuildDate == "" {
		result.BuildDate = "unknown"
	}
	return result
}

func runVersion(args []string) error {
	flags := flag.NewFlagSet("notch version", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	jsonOutput := flags.Bool("json", false, "emit version details as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: notch version [--json]")
	}
	info := currentBuildInfo()
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(info)
	}
	fmt.Printf("notch %s\ncommit: %s\nbuilt: %s\ngo: %s\nplatform: %s\n", info.Version, info.Commit, info.BuildDate, info.GoVersion, info.Platform)
	return nil
}

func runUpgrade(args []string) error {
	flags := flag.NewFlagSet("notch upgrade", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	checkOnly := flags.Bool("check", false, "check for an update without installing it")
	force := flags.Bool("force", false, "reinstall or allow an explicit downgrade")
	target := flags.String("version", "", "install a specific release version")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: notch upgrade [--check] [--force] [--version VERSION]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	info := currentBuildInfo()
	result, err := upgrade.Run(ctx, upgrade.Options{
		CurrentVersion: info.Version, TargetVersion: *target,
		CheckOnly: *checkOnly, Force: *force,
	})
	if err != nil {
		return err
	}
	if result.Updated {
		fmt.Printf("Upgraded notch from %s to %s.\n", result.CurrentVersion, result.TargetVersion)
	} else if result.Available {
		fmt.Printf("Update available: %s -> %s\n", result.CurrentVersion, result.TargetVersion)
	} else {
		fmt.Printf("notch %s is up to date.\n", result.CurrentVersion)
	}
	return nil
}

func terminalsInteractive(in, out *os.File) bool {
	return term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

func resolveWorkspaceTrust(home, root, trustKey string, opts options, in io.Reader, diagnostic io.Writer, interactive bool) (bool, error) {
	if opts.safe {
		return false, nil
	}
	dataRoot, err := config.DataDir(home)
	if err != nil {
		return false, err
	}
	store := workspace.NewStore(dataRoot)
	if opts.trustWorkspace {
		if err := store.TrustRoot(trustKey); err != nil {
			return false, err
		}
		return true, nil
	}
	hasInputs, err := workspace.HasProjectInputs(root)
	if err != nil {
		return false, err
	}
	if !hasInputs {
		return false, nil
	}
	trusted, err := store.IsTrustedWorkspace(root, trustKey)
	if err != nil {
		// --safe must remain usable as an emergency bypass, but an unreadable or
		// insecure trust database must never silently downgrade a normal run.
		return false, err
	}
	if trusted {
		return true, nil
	}
	if !interactive {
		return false, nil
	}
	fmt.Fprintf(diagnostic, "Trust workspace %s and load its project config, resources, extensions, and AGENTS instructions? [y/N] ", root)
	reader := bufio.NewReader(in)
	answer, readErr := reader.ReadString('\n')
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return false, fmt.Errorf("read workspace trust response: %w", readErr)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		if err := store.TrustRoot(trustKey); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, nil
	}
}

func runListModels(args []string) error {
	flags := flag.NewFlagSet("notch models", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	force := flags.Bool("refresh", false, "refresh the provider model cache")
	jsonOutput := flags.Bool("json", false, "emit a stable JSON model catalog")
	all := flags.Bool("all", false, "list models from every supported provider")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 || (*all && flags.NArg() != 0) {
		return errors.New("usage: notch models [--refresh] [--json] [--all | provider]")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cfg, err := config.LoadGlobal(home)
	if err != nil {
		return err
	}
	providers := []string{cfg.Provider}
	if flags.NArg() == 1 {
		providers[0] = flags.Arg(0)
	} else if *all {
		providers = modelregistry.Providers()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	registry := modelRegistryFor(cfg)
	store := credentials.New(cfg.AuthFile)
	var models []modelregistry.Entry
	var listErrors []error
	for _, providerName := range providers {
		entries, listErr := discoverModels(ctx, registry, cfg, store, providerName, *force)
		models = append(models, entries...)
		if listErr != nil {
			listErrors = append(listErrors, fmt.Errorf("%s: %w", normalizeProvider(providerName), listErr))
			if len(entries) != 0 {
				fmt.Fprintln(os.Stderr, "notch: model registry:", listErr, "(showing fallback data)")
			}
		}
	}
	if len(models) == 0 {
		if err := errors.Join(listErrors...); err != nil {
			return err
		}
		return errors.New("no models found")
	}
	return writeModelList(os.Stdout, models, *jsonOutput)
}

func writeModelList(w io.Writer, models []modelregistry.Entry, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(w).Encode(struct {
			Version int                   `json:"version"`
			Models  []modelregistry.Entry `json:"models"`
		}{Version: 1, Models: models})
	}
	for _, entry := range models {
		contextText := "-"
		if entry.ContextWindow > 0 {
			contextText = fmt.Sprintf("%d", entry.ContextWindow)
		}
		reasoning := ""
		if entry.Reasoning {
			reasoning = "reasoning"
		}
		if _, err := fmt.Fprintf(w, "%-16s %-42s %-10s %-10s %s\n", entry.Provider, entry.ID, contextText, reasoning, entry.Name); err != nil {
			return err
		}
	}
	return nil
}

func makeProvider(ctx context.Context, cfg config.Config, store *credentials.Store) (model.Provider, error) {
	provider := normalizeProvider(cfg.Provider)
	switch provider {
	case "anthropic":
		if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
			return anthropic.New(anthropic.Config{APIKey: key, BaseURL: cfg.BaseURL}), nil
		}
		return nil, errors.New("ANTHROPIC_API_KEY is not set")
	case "anthropic-claude-code":
		if token := os.Getenv("ANTHROPIC_OAUTH_TOKEN"); token != "" {
			return anthropic.New(anthropic.Config{OAuthToken: token, OAuthMode: true, BaseURL: cfg.BaseURL}), nil
		}
		authorizer := newAuthorizer(store, provider)
		if _, err := authorizer.Credential(ctx); err != nil {
			return nil, err
		}
		return anthropic.New(anthropic.Config{Authorize: authorizer.Token, OAuthMode: true, BaseURL: cfg.BaseURL}), nil
	case "openai-codex":
		authorizer := newAuthorizer(store, provider)
		credential, err := authorizer.Credential(ctx)
		if err != nil {
			return nil, err
		}
		return codex.New(codex.Config{Authorize: authorizer.Token, AccountID: credential.AccountID, BaseURL: cfg.BaseURL}), nil
	case "openrouter":
		key := os.Getenv("OPENROUTER_API_KEY")
		if key == "" {
			credential, err := resolveCredential(ctx, store, provider)
			if err != nil {
				return nil, err
			}
			key = credential.Access
		}
		return openrouter.New(openrouter.Config{APIKey: key, BaseURL: cfg.BaseURL, AppName: "Notch"}), nil
	case "openai":
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" && cfg.BaseURL == "" {
			return nil, errors.New("OPENAI_API_KEY is not set")
		}
		return openai.New(openai.Config{APIKey: key, BaseURL: cfg.BaseURL}), nil
	default:
		return nil, fmt.Errorf("unsupported provider %q (use openai-codex, anthropic-claude-code, openrouter, anthropic, or openai)", cfg.Provider)
	}
}

func enableStartupPlanMode(ctx context.Context, registry *extension.Registry) error {
	command, ok := registry.Command("plan")
	if !ok {
		return errors.New("--plan requires the official plan extension")
	}
	if _, err := command.Execute(ctx, "on"); err != nil {
		return fmt.Errorf("enable plan mode: %w", err)
	}
	return nil
}

func reapplyToolPolicy(registry *extension.Registry, opts options) {
	if opts.noTools {
		registry.RestrictTools(nil)
		return
	}
	if opts.toolAllow != "" {
		allowed, err := parseToolNames(opts.toolAllow)
		if err == nil {
			registry.RestrictTools(allowed)
		}
	}
	if opts.toolExclude != "" {
		excluded, err := parseToolNames(opts.toolExclude)
		if err == nil {
			registry.RemoveTools(excluded)
		}
	}
}

func applyToolPolicy(registry *extension.Registry, opts options) error {
	if opts.noTools {
		registry.RestrictTools(nil)
		return nil
	}
	if opts.toolAllow != "" {
		allowed, err := parseToolNames(opts.toolAllow)
		if err != nil {
			return fmt.Errorf("--tools: %w", err)
		}
		if missing := registry.RestrictTools(allowed); len(missing) != 0 {
			return fmt.Errorf("--tools references unavailable tools: %s", strings.Join(missing, ", "))
		}
	}
	if opts.toolExclude != "" {
		excluded, err := parseToolNames(opts.toolExclude)
		if err != nil {
			return fmt.Errorf("--exclude-tools: %w", err)
		}
		if missing := registry.RemoveTools(excluded); len(missing) != 0 {
			return fmt.Errorf("--exclude-tools references unavailable tools: %s", strings.Join(missing, ", "))
		}
	}
	return nil
}

func parseToolNames(value string) ([]string, error) {
	parts := strings.Split(value, ",")
	names := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, errors.New("tool names cannot be empty")
		}
		if !seen[name] {
			seen[name], names = true, append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func rpcAPIForProvider(provider string) string {
	switch normalizeProvider(provider) {
	case "anthropic", "anthropic-claude-code":
		return "anthropic-messages"
	case "openrouter":
		return "openai-completions"
	case "openai-codex":
		return "openai-codex-responses"
	default:
		return "openai-responses"
	}
}

func validThinkingLevel(level string) bool {
	switch level {
	case "off", "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func tuiPresets(presets map[string]config.PresetConfig) map[string]tui.ModelPreset {
	if len(presets) == 0 {
		return nil
	}
	result := make(map[string]tui.ModelPreset, len(presets))
	for key, preset := range presets {
		result[key] = tui.ModelPreset{
			Provider: normalizeProvider(preset.Provider), Model: preset.Model, ThinkingLevel: preset.ThinkingLevel,
		}
	}
	return result
}

func normalizeProvider(provider string) string {
	switch strings.ToLower(provider) {
	case "codex", "chatgpt":
		return "openai-codex"
	case "claude":
		return "anthropic-claude-code"
	default:
		return strings.ToLower(provider)
	}
}

func defaultModelFor(provider string) string {
	switch normalizeProvider(provider) {
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

func contextWindowFor(provider, modelName string) int {
	provider, modelName = normalizeProvider(provider), strings.ToLower(modelName)
	switch provider {
	case "openai-codex":
		return 272000
	case "anthropic", "anthropic-claude-code":
		if strings.Contains(modelName, "claude-") {
			return 1000000
		}
		return 200000
	case "openrouter":
		return 128000
	default:
		return 128000
	}
}

// newAuthorizer returns the OAuth token source for provider. The authorizer
// re-reads and refreshes the stored credential for the life of the process, so
// a session outliving its access token recovers without a restart.
func newAuthorizer(store *credentials.Store, provider string) *providerauth.Authorizer {
	legacyProvider := ""
	if provider == credentials.AnthropicClaudeCodeProvider {
		legacyProvider = credentials.LegacyAnthropicProvider
	}
	return providerauth.New(store, provider, legacyProvider, oauth.Refresh)
}

func resolveCredential(ctx context.Context, store *credentials.Store, provider string) (credentials.Credential, error) {
	return newAuthorizer(store, provider).Credential(ctx)
}

func runAuth(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// Authentication never reads project configuration, even for a workspace
	// that has previously been trusted.
	cfg, err := config.LoadGlobal(home)
	if err != nil {
		return err
	}
	store := credentials.New(cfg.AuthFile)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "login":
		if len(args) != 2 {
			return errors.New("usage: notch login <openai-codex|anthropic-claude-code|openrouter>")
		}
		provider := normalizeProvider(args[1])
		credential, err := oauth.Login(ctx, provider, os.Stderr)
		if err != nil {
			return err
		}
		if err := store.Put(provider, credential); err != nil {
			return err
		}
		fmt.Printf("logged in to %s; credentials saved to %s\n", provider, store.Path())
		return nil
	case "logout":
		if len(args) != 2 {
			return errors.New("usage: notch logout <provider>")
		}
		provider := normalizeProvider(args[1])
		if err := store.Delete(provider); err != nil {
			return err
		}
		if provider == credentials.AnthropicClaudeCodeProvider {
			// Remove the retained legacy credential too, otherwise the next use
			// would migrate it back and effectively undo logout.
			if err := store.Delete(credentials.LegacyAnthropicProvider); err != nil {
				return err
			}
		}
		fmt.Println("removed", provider, "credential")
		return nil
	case "auth":
		if len(args) < 2 {
			return errors.New("usage: notch auth <status|import-pi>")
		}
		switch args[1] {
		case "import-pi":
			path := filepath.Join(home, ".pi", "agent", "auth.json")
			if len(args) > 2 {
				path = args[2]
			}
			if err := store.ImportPi(path); err != nil {
				return err
			}
			fmt.Printf("imported Pi credentials into %s\n", store.Path())
			return nil
		case "status":
			for _, provider := range []string{"openai-codex", "anthropic-claude-code", "openrouter"} {
				credential, ok, getErr := store.Get(provider)
				if provider == credentials.AnthropicClaudeCodeProvider {
					credential, ok, getErr = store.GetWithLegacyFallback(provider, credentials.LegacyAnthropicProvider)
				}
				if getErr != nil {
					return getErr
				}
				status := "not logged in"
				if ok && credential.Access != "" {
					status = "stored"
					if credential.Expires > 0 {
						status += ", expires " + time.UnixMilli(credential.Expires).Format(time.RFC3339)
					}
				}
				fmt.Printf("%s: %s\n", provider, status)
			}
			return nil
		default:
			return fmt.Errorf("unknown auth command %q", args[1])
		}
	}
	return fmt.Errorf("unknown auth command %q", args[0])
}

func handleCommand(ctx context.Context, input string, registry *extension.Registry, catalog *resources.Catalog, terminal *ui.Terminal, runner *agent.Agent) (handled, exit bool, err error) {
	name, args, _ := strings.Cut(strings.TrimPrefix(input, "/"), " ")
	switch name {
	case "exit", "quit":
		return true, true, nil
	case "help":
		fmt.Fprintln(os.Stderr, "/help  /tools  /skills  /new  /compact  /thinking  /exit")
		for _, command := range registry.Commands() {
			fmt.Fprintf(os.Stderr, "/%s — %s\n", command.Name, command.Description)
		}
		return true, false, nil
	case "tools":
		for _, tool := range registry.Tools() {
			fmt.Fprintf(os.Stderr, "%s\t%s\n", tool.Definition.Name, tool.Source)
		}
		return true, false, nil
	case "new":
		_, resetErr := runner.ResetConversation(nil)
		if resetErr == nil {
			fmt.Fprintln(os.Stderr, "new conversation")
		}
		return true, false, resetErr
	case "compact":
		compactErr := runner.Compact(ctx, strings.TrimSpace(args), false, terminal.Render)
		if compactErr == nil {
			fmt.Fprintln(os.Stderr, "context compacted")
		}
		if errors.Is(compactErr, agent.ErrNothingToCompact) {
			fmt.Fprintln(os.Stderr, "nothing to compact")
			compactErr = nil
		}
		return true, false, compactErr
	case "thinking":
		level := strings.TrimSpace(args)
		if level == "" {
			fmt.Fprintln(os.Stderr, runner.ThinkingLevel())
			return true, false, nil
		}
		thinkingErr := runner.SetThinkingLevel(level)
		if thinkingErr == nil {
			fmt.Fprintln(os.Stderr, "thinking level:", level)
		}
		return true, false, thinkingErr
	case "skills":
		names := make([]string, 0, len(catalog.Skills))
		for n := range catalog.Skills {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(os.Stderr, "/skill:%s\t%s\n", n, catalog.Skills[n].Description)
		}
		return true, false, nil
	}
	if command, ok := registry.Command(name); ok {
		result, commandErr := command.Execute(ctx, strings.TrimSpace(args))
		if result != "" {
			fmt.Fprintln(os.Stderr, result)
		}
		return true, false, commandErr
	}
	return false, false, nil
}

func initialize(home, cwd string, cfg config.Config) error {
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	root, err := config.ConfigDir(home)
	if err != nil {
		return err
	}
	path := filepath.Join(root, "config.json")
	if _, err := os.Stat(path); err == nil {
		fmt.Println(path, "already exists")
		return nil
	}
	data, _ := json.MarshalIndent(map[string]any{
		"provider": cfg.Provider, "model": cfg.Model, "explore_model": cfg.ExploreModel, "max_tokens": cfg.MaxTokens,
		"theme": cfg.Theme, "thinking_level": cfg.ThinkingLevel, "auto_update": cfg.AutoUpdate, "compaction": cfg.Compaction,
	}, "", "  ")
	data = append(data, '\n')
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	fmt.Println("created", path)
	fmt.Println("project extensions:", filepath.Join(cwd, ".notch", "extensions"))
	fmt.Println("project themes:", filepath.Join(cwd, ".notch", "themes"))
	return nil
}
