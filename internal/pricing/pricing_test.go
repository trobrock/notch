package pricing

import (
	"math"
	"testing"

	"github.com/trobrock/notch/internal/model"
)

func TestEstimateAnthropicShortAndLongCacheWrites(t *testing.T) {
	usage := model.Response{InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadTokens: 500_000, CacheWriteTokens: 200_000}
	short, ok := Estimate("anthropic", "claude-sonnet-4-5", "short", usage)
	if !ok || math.Abs(short-5.4) > 1e-9 {
		t.Fatalf("short estimate = %v, %v", short, ok)
	}
	long, ok := Estimate("anthropic-claude-code", "claude-sonnet-4-5-20250929", "long", usage)
	if !ok || math.Abs(long-5.85) > 1e-9 {
		t.Fatalf("long estimate = %v, %v", long, ok)
	}
}

func TestEstimateOpenAITierAndUnknownModel(t *testing.T) {
	usage := model.Response{InputTokens: 300_000, OutputTokens: 10_000, CacheReadTokens: 50_000}
	cost, ok := Estimate("openai-codex", "gpt-5.4", "short", usage)
	if !ok || math.Abs(cost-1.75) > 1e-9 {
		t.Fatalf("tier estimate = %v, %v", cost, ok)
	}
	if _, ok := Estimate("openai", "unknown", "short", usage); ok {
		t.Fatal("unknown model unexpectedly estimated")
	}
	if _, ok := Estimate("openrouter", "openai/gpt-5", "short", usage); ok {
		t.Fatal("OpenRouter must use provider-reported cost")
	}
}
