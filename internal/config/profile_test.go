package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveProfileAppliesRoutingAndAllowlist(t *testing.T) {
	cfg := Config{Provider: "openai", Model: "old", BaseURL: "https://old.example", ThinkingLevel: "low", Profiles: map[string]ProfileConfig{
		"work": {Provider: "anthropic-claude-code", Model: "configured-model", ThinkingLevel: "high", AllowedProviders: []string{"anthropic-claude-code"}, CredentialRef: "anthropic-claude-code:work"},
	}}
	snapshot, err := ResolveProfile(&cfg, "WORK")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "anthropic-claude-code" || cfg.Model != "configured-model" || cfg.BaseURL != "" || cfg.CredentialRef != "anthropic-claude-code:work" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if snapshot.Profile != "work" || snapshot.DefinitionHash == "" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if err := snapshot.ValidateProvider("openai"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("error = %v", err)
	}
	if err := snapshot.ValidateProvider("anthropic-claude-code"); err != nil {
		t.Fatal(err)
	}
}

func TestResolveProfileErrorsAreActionable(t *testing.T) {
	cfg := Config{Profiles: map[string]ProfileConfig{"bad": {Provider: "anthropic", Model: "", AllowedProviders: []string{"anthropic"}}}}
	if _, err := ResolveProfile(&cfg, "missing"); err == nil || !strings.Contains(err.Error(), "available") {
		t.Fatalf("error = %v", err)
	}
	if _, err := ResolveProfile(&cfg, "bad"); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("error = %v", err)
	}
}

func TestRoutingSnapshotJSONContainsNoSecrets(t *testing.T) {
	s := RoutingSnapshot{Profile: "personal", Provider: "anthropic", Model: "configured-model", AllowedProviders: []string{"anthropic"}, CredentialRef: "personal-api", DefinitionHash: "hash"}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sk-secret") {
		t.Fatal("snapshot contained a secret")
	}
}

func TestProfileAppliesAfterEnvironmentAndProjectCannotDefineProfiles(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	configDir := filepath.Join(home, "notch")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	global := `{"profiles":{"work":{"provider":"anthropic-claude-code","model":"configured-work-model","allowed_providers":["anthropic-claude-code"]}}}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOTCH_PROVIDER", "openai")
	t.Setenv("NOTCH_MODEL", "environment-model")
	cfg, err := LoadWorkspace(home, project, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "openai" {
		t.Fatalf("environment provider = %q", cfg.Provider)
	}
	if _, err := ResolveProfile(&cfg, "work"); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "anthropic-claude-code" || cfg.Model != "configured-work-model" {
		t.Fatalf("profile did not override environment: %+v", cfg)
	}

	projectNotch := filepath.Join(project, ".notch")
	if err := os.MkdirAll(projectNotch, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectNotch, "config.json"), []byte(`{"profiles":{"work":{"provider":"openai","model":"bad","allowed_providers":["openai"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspace(home, project, true); err == nil || !strings.Contains(err.Error(), "global-only") {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigCannotSelectProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	configDir := filepath.Join(home, "config", "notch")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"profile":"work"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(home); err == nil || !strings.Contains(err.Error(), "use --profile") {
		t.Fatalf("error = %v", err)
	}
}

func TestProfileNamesRejectCaseInsensitiveCollisions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	configDir := filepath.Join(home, "config", "notch")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"profiles":{"Work":{"provider":"anthropic","model":"one","allowed_providers":["anthropic"]},"work":{"provider":"openai","model":"two","allowed_providers":["openai"]}}}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(home); err == nil || !strings.Contains(err.Error(), "differ only by case") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveProfileRejectsUnsupportedProviders(t *testing.T) {
	for name, profile := range map[string]ProfileConfig{
		"primary": {Provider: "made-up", Model: "model", AllowedProviders: []string{"made-up"}},
		"allowed": {Provider: "anthropic", Model: "model", AllowedProviders: []string{"anthropic", "made-up"}},
	} {
		cfg := Config{ThinkingLevel: "medium", Profiles: map[string]ProfileConfig{name: profile}}
		if _, err := ResolveProfile(&cfg, name); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("%s error = %v", name, err)
		}
	}
}

func TestProfileNamesRejectExactJSONDuplicates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	configDir := filepath.Join(home, "config", "notch")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"profiles":{"work":{"provider":"anthropic","model":"one"},"work":{"provider":"openai","model":"two"}}}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(home); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("error = %v", err)
	}
}

func TestProfileCredentialReferenceMustMatchProvider(t *testing.T) {
	cfg := Config{ThinkingLevel: "medium", Profiles: map[string]ProfileConfig{"work": {Provider: "openai", Model: "model", CredentialRef: "anthropic-claude-code", AllowedProviders: []string{"openai"}}}}
	if _, err := ResolveProfile(&cfg, "work"); err == nil || !strings.Contains(err.Error(), "credential_ref") {
		t.Fatalf("error = %v", err)
	}
	cfg.Profiles["work"] = ProfileConfig{Provider: "openai", Model: "model", CredentialRef: "openai:work", AllowedProviders: []string{"openai"}}
	if _, err := ResolveProfile(&cfg, "work"); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRoutingSnapshotRejectsCrossProviderCredentialReference(t *testing.T) {
	cfg := Config{}
	snapshot := RoutingSnapshot{Profile: "work", Provider: "openai", Model: "model", ThinkingLevel: "medium", AllowedProviders: []string{"openai"}, CredentialRef: "openrouter:work", DefinitionHash: "hash"}
	if err := ApplyRoutingSnapshot(&cfg, snapshot); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("error = %v", err)
	}
}

func TestProfileDuplicateDetectionMatchesJSONFieldCaseInsensitively(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	configDir := filepath.Join(home, "config", "notch")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"PROFILES":{"work":{"provider":"anthropic","model":"one"},"work":{"provider":"openai","model":"two"}}}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(home); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("error = %v", err)
	}
}
