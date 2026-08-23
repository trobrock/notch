package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/config"
	"github.com/trobrock/notch/internal/credentials"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/luaext"
	"github.com/trobrock/notch/internal/mcp"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/oauth"
	"github.com/trobrock/notch/internal/provider/anthropic"
	"github.com/trobrock/notch/internal/provider/codex"
	"github.com/trobrock/notch/internal/provider/openai"
	"github.com/trobrock/notch/internal/provider/openrouter"
	"github.com/trobrock/notch/internal/resources"
	"github.com/trobrock/notch/internal/session"
	"github.com/trobrock/notch/internal/tools"
	"github.com/trobrock/notch/internal/ui"
)

var version = "dev"

type options struct {
	provider, modelName, prompt, mcpConfig                    string
	continueSession, noSession, jsonOutput, init, showVersion bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "notch:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && (args[0] == "login" || args[0] == "logout" || args[0] == "auth") {
		return runAuth(args)
	}
	var opts options
	flags := flag.NewFlagSet("notch", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&opts.provider, "provider", "", "provider: openai-codex, openrouter, anthropic, or openai")
	flags.StringVar(&opts.provider, "p", "", "model provider (shorthand)")
	flags.StringVar(&opts.modelName, "model", "", "model ID")
	flags.StringVar(&opts.modelName, "m", "", "model ID (shorthand)")
	flags.StringVar(&opts.prompt, "print", "", "run one prompt and exit")
	flags.StringVar(&opts.mcpConfig, "mcp-config", "", "path to MCP JSON config")
	flags.BoolVar(&opts.continueSession, "continue", false, "continue the latest session")
	flags.BoolVar(&opts.noSession, "no-session", false, "do not save a session")
	flags.BoolVar(&opts.jsonOutput, "json", false, "emit JSONL events")
	flags.BoolVar(&opts.init, "init", false, "create ~/.notch and a starter config")
	flags.BoolVar(&opts.showVersion, "version", false, "print version")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if opts.showVersion {
		fmt.Println("notch", version)
		return nil
	}
	if opts.prompt == "" && flags.NArg() != 0 {
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
	cfg, err := config.Load(home, cwd)
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
	if opts.mcpConfig != "" {
		cfg.MCPConfig = opts.mcpConfig
	}
	if opts.init {
		return initialize(home, cwd, cfg)
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	terminal := ui.DefaultTerminal(cwd)
	registry := extension.NewRegistry()
	if err := tools.RegisterBuiltins(registry, cwd); err != nil {
		return err
	}

	plugins, warnings := extension.DiscoverAndLoad(ctx, cfg.ExtensionDirs, registry, terminal)
	defer func() {
		for i := len(plugins) - 1; i >= 0; i-- {
			_ = plugins[i].Close()
		}
	}()
	for _, warning := range warnings {
		terminal.Notify(warning.Error(), "warning")
	}

	luaManager := luaext.New(registry, terminal)
	if err := luaManager.LoadDirs(cfg.ExtensionDirs...); err != nil {
		terminal.Notify(err.Error(), "warning")
	}
	defer luaManager.Close()

	var mcpManager *mcp.Manager
	if cfg.MCPConfig != "" {
		if _, statErr := os.Stat(cfg.MCPConfig); statErr == nil {
			mcpCfg, loadErr := mcp.LoadConfig(cfg.MCPConfig)
			if loadErr != nil {
				terminal.Notify(loadErr.Error(), "warning")
			} else if mcpManager, err = mcp.ConnectConfigured(ctx, mcpCfg, registry); err != nil {
				terminal.Notify(err.Error(), "warning")
			}
		}
	}
	if mcpManager != nil {
		defer mcpManager.Close()
	}

	catalog, err := resources.Load(cfg.SkillDirs, cfg.PromptDirs)
	if err != nil {
		terminal.Notify(err.Error(), "warning")
	}
	systemPrompt := cfg.SystemPrompt
	if summary := catalog.SystemSummary(); summary != "" {
		systemPrompt += "\n\n" + summary
	}

	credentialStore := credentials.New(cfg.AuthFile)
	provider, err := makeProvider(ctx, cfg, credentialStore)
	if err != nil {
		return err
	}
	var store *session.Session
	if !opts.noSession {
		if opts.continueSession {
			store, err = session.Latest(cfg.SessionDir)
		} else {
			store, err = session.New(cfg.SessionDir, cwd, cfg.Provider, cfg.Model)
		}
		if err != nil {
			return err
		}
		defer store.Close()
	}
	runner, err := agent.New(agent.Config{Provider: provider, Registry: registry, Session: store, Model: cfg.Model, SystemPrompt: systemPrompt, MaxTokens: cfg.MaxTokens})
	if err != nil {
		return err
	}

	emit := terminal.Render
	if opts.jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		emit = func(event agent.Event) { _ = encoder.Encode(event) }
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
		fmt.Fprintf(os.Stderr, "Notch %s · %s/%s · %d tools\n", version, cfg.Provider, cfg.Model, len(registry.Tools()))
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
		if handled, exit, commandErr := handleCommand(ctx, input, registry, catalog, terminal); handled {
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

func makeProvider(ctx context.Context, cfg config.Config, store *credentials.Store) (model.Provider, error) {
	provider := normalizeProvider(cfg.Provider)
	switch provider {
	case "anthropic":
		if token := os.Getenv("ANTHROPIC_OAUTH_TOKEN"); token != "" {
			return anthropic.New(anthropic.Config{OAuthToken: token, OAuthMode: true, BaseURL: cfg.BaseURL}), nil
		}
		if _, ok, getErr := store.Get(provider); getErr != nil {
			return nil, getErr
		} else if ok {
			credential, err := resolveCredential(ctx, store, provider)
			if err != nil {
				return nil, err
			}
			return anthropic.New(anthropic.Config{OAuthToken: credential.Access, OAuthMode: true, BaseURL: cfg.BaseURL}), nil
		}
		if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
			return anthropic.New(anthropic.Config{APIKey: key, BaseURL: cfg.BaseURL}), nil
		}
		return nil, errors.New("no Anthropic credential; run: notch login anthropic, or set ANTHROPIC_API_KEY")
	case "openai-codex":
		credential, err := resolveCredential(ctx, store, provider)
		if err != nil {
			return nil, err
		}
		return codex.New(codex.Config{AccessToken: credential.Access, AccountID: credential.AccountID, BaseURL: cfg.BaseURL}), nil
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
		return nil, fmt.Errorf("unsupported provider %q (use openai-codex, openrouter, anthropic, or openai)", cfg.Provider)
	}
}

func normalizeProvider(provider string) string {
	switch strings.ToLower(provider) {
	case "codex", "chatgpt":
		return "openai-codex"
	case "claude":
		return "anthropic"
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
	case "anthropic":
		return "claude-sonnet-4-5"
	default:
		return "gpt-5"
	}
}

func resolveCredential(ctx context.Context, store *credentials.Store, provider string) (credentials.Credential, error) {
	credential, ok, err := store.Get(provider)
	if err != nil {
		return credentials.Credential{}, err
	}
	if !ok || credential.Access == "" {
		return credentials.Credential{}, fmt.Errorf("no %s credential; run: notch login %s", provider, provider)
	}
	if credential.Refresh != "" && credential.Expires > 0 && credential.Expires <= time.Now().Add(5*time.Minute).UnixMilli() {
		credential, err = oauth.Refresh(ctx, provider, credential)
		if err != nil {
			return credentials.Credential{}, fmt.Errorf("refresh %s login: %w", provider, err)
		}
		if err := store.Put(provider, credential); err != nil {
			return credentials.Credential{}, err
		}
	}
	return credential, nil
}

func runAuth(args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cfg, err := config.Load(home, cwd)
	if err != nil {
		return err
	}
	store := credentials.New(cfg.AuthFile)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "login":
		if len(args) != 2 {
			return errors.New("usage: notch login <openai-codex|anthropic|openrouter>")
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
			for _, provider := range []string{"openai-codex", "anthropic", "openrouter"} {
				credential, ok, getErr := store.Get(provider)
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

func handleCommand(ctx context.Context, input string, registry *extension.Registry, catalog *resources.Catalog, terminal *ui.Terminal) (handled, exit bool, err error) {
	name, args, _ := strings.Cut(strings.TrimPrefix(input, "/"), " ")
	switch name {
	case "exit", "quit":
		return true, true, nil
	case "help":
		fmt.Fprintln(os.Stderr, "/help  /tools  /skills  /exit")
		for _, command := range registry.Commands() {
			fmt.Fprintf(os.Stderr, "/%s — %s\n", command.Name, command.Description)
		}
		return true, false, nil
	case "tools":
		for _, tool := range registry.Tools() {
			fmt.Fprintf(os.Stderr, "%s\t%s\n", tool.Definition.Name, tool.Source)
		}
		return true, false, nil
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
	root := os.Getenv("NOTCH_HOME")
	if root == "" {
		root = filepath.Join(home, ".notch")
	}
	path := filepath.Join(root, "config.json")
	if _, err := os.Stat(path); err == nil {
		fmt.Println(path, "already exists")
		return nil
	}
	data, _ := json.MarshalIndent(map[string]any{
		"provider": cfg.Provider, "model": cfg.Model, "max_tokens": cfg.MaxTokens,
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
	return nil
}
