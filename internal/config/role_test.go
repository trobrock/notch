package config

import (
	"strings"
	"testing"
)

func TestRoleResolutionUsesProfileMappingAndAlias(t *testing.T) {
	cfg := Config{Roles: map[string]RoleConfig{"auditor": {Instructions: "Check policy.", Model: "base-audit", Thinking: "low"}}}
	personal := &RoutingSnapshot{Profile: "personal", Provider: "openai", AllowedProviders: []string{"openai"}, RoleModels: map[string]string{"reviewer": "review-mini"}}
	work := &RoutingSnapshot{Profile: "work", Provider: "openai", AllowedProviders: []string{"openai"}, RoleModels: map[string]string{"reviewer": "review-full"}}
	a, err := ResolveRole(cfg, personal, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ResolveRole(cfg, work, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if a.Instructions != b.Instructions || a.Model != "review-mini" || b.Model != "review-full" || a.ModelProvenance != "profile role mapping" {
		t.Fatalf("personal=%#v work=%#v", a, b)
	}
	developer, err := ResolveRole(cfg, nil, "implementer")
	if err != nil || developer.Name != "developer" || developer.RequestedName != "implementer" {
		t.Fatalf("alias=%#v err=%v", developer, err)
	}
	custom, err := ResolveRole(cfg, nil, "auditor")
	if err != nil || custom.Source != "user config" || custom.Instructions != "Check policy." {
		t.Fatalf("custom=%#v err=%v", custom, err)
	}
}

func TestProfileRoleMappingIsHashedAndCannotEscapeProviderBoundary(t *testing.T) {
	cfg := Config{ThinkingLevel: "medium", Profiles: map[string]ProfileConfig{"work": {
		Provider: "openai", Model: "base", AllowedProviders: []string{"openai"}, RoleModels: map[string]string{"researcher": "openai/research-mini"},
	}}}
	first, err := ResolveProfile(&cfg, "work")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles["work"] = ProfileConfig{Provider: "openai", Model: "base", AllowedProviders: []string{"openai"}, RoleModels: map[string]string{"researcher": "openai/research-full"}}
	second, err := ResolveProfile(&cfg, "work")
	if err != nil {
		t.Fatal(err)
	}
	if first.DefinitionHash == second.DefinitionHash {
		t.Fatal("role mapping drift did not change profile hash")
	}
	cfg.Profiles["work"] = ProfileConfig{Provider: "openai", Model: "base", AllowedProviders: []string{"openai"}, RoleModels: map[string]string{"researcher": "anthropic/claude"}}
	if _, err := ResolveProfile(&cfg, "work"); err == nil || !strings.Contains(err.Error(), "outside allowed_providers") {
		t.Fatalf("err=%v", err)
	}
}

func TestRoleDefaultsValidate(t *testing.T) {
	cfg := Config{Roles: map[string]RoleConfig{"bad": {Instructions: "x", Thinking: "enormous"}}}
	if _, err := ResolveRole(cfg, nil, "bad"); err == nil || !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("err=%v", err)
	}
}
