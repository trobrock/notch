package modelregistry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/model"
)

func TestRegistryRefreshCacheAndFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	registry := New(path, time.Hour)
	registry.now = func() time.Time { return now }
	calls := 0
	fetch := func(context.Context) ([]model.ModelInfo, error) {
		calls++
		return []model.ModelInfo{{ID: "z-model", Name: "Zed", ContextWindow: 42}, {ID: "a-model", Reasoning: true}, {ID: "a-model", Name: "A"}}, nil
	}
	models, err := registry.List(context.Background(), "openai", Scope("openai", "https://one.test"), false, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(models) != 2 || models[0].ID != "a-model" || models[0].Name != "A" || models[0].Source != "provider" {
		t.Fatalf("refreshed models = %#v, calls=%d", models, calls)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %v, %v", info, err)
	}
	models, err = registry.List(context.Background(), "openai", Scope("openai", "https://one.test"), false, func(context.Context) ([]model.ModelInfo, error) {
		t.Fatal("fresh cache fetched")
		return nil, nil
	})
	if err != nil || len(models) != 2 {
		t.Fatalf("fresh cache = %#v, %v", models, err)
	}

	now = now.Add(2 * time.Hour)
	models, err = registry.List(context.Background(), "openai", Scope("openai", "https://one.test"), false, func(context.Context) ([]model.ModelInfo, error) {
		return nil, errors.New("offline")
	})
	if err == nil || len(models) != 2 || models[0].Source != "provider" {
		t.Fatalf("stale fallback = %#v, %v", models, err)
	}
}

func TestRegistryBuiltinAndEndpointScopes(t *testing.T) {
	registry := New("", time.Hour)
	for _, provider := range []string{"openai-codex", "anthropic", "anthropic-claude-code"} {
		models, err := registry.List(context.Background(), provider, provider, false, nil)
		if err != nil || len(models) < 3 || models[0].Source != "bundled" {
			t.Fatalf("builtin %s = %#v, %v", provider, models, err)
		}
	}
	apiModels := Builtin("anthropic")
	oauthModels := Builtin("anthropic-claude-code")
	if len(apiModels) != len(oauthModels) {
		t.Fatalf("anthropic catalogs differ in length: %d vs %d", len(apiModels), len(oauthModels))
	}
	apiModels[0].Name = "mutated"
	if oauthModels[0].Name == "mutated" {
		t.Fatal("anthropic bundled catalogs alias each other")
	}
	if Scope("openai", "https://one.test/") == Scope("openai", "https://two.test") || Scope("openai", "") != "openai" {
		t.Fatal("cache scopes do not separate endpoints")
	}
}
