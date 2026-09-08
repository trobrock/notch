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

func TestEstimateClaudeFable(t *testing.T) {
	cost, ok := Estimate("anthropic", "claude-fable-5-1", "short", model.Response{InputTokens: 1_000_000, OutputTokens: 100_000})
	if !ok || math.Abs(cost-3) > 1e-9 {
		t.Fatalf("estimate = %v, %v", cost, ok)
	}
}

func TestEstimateAstraStandardPricing(t *testing.T) {
	for _, provider := range []string{"openai", "openai-codex"} {
		for _, tc := range []struct {
			name  string
			usage model.Response
			want  float64
		}{
			{"input", model.Response{InputTokens: 1_000_000}, 10},
			{"cached", model.Response{CacheReadTokens: 1_000_000}, 1},
			{"output", model.Response{OutputTokens: 1_000_000}, 50},
			{"mixed", model.Response{InputTokens: 1_000_000, CacheReadTokens: 500_000, OutputTokens: 100_000, ReasoningTokens: 20_000}, 15.5},
			{"pilot", model.Response{InputTokens: 21_578, OutputTokens: 541}, 0.24283},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				cost, ok := Estimate(provider, "gpt-6-astra", "short", tc.usage)
				if !ok || math.Abs(cost-tc.want) > 1e-9 {
					t.Fatalf("cost=%v known=%v want=%v", cost, ok, tc.want)
				}
			})
		}
	}
}
