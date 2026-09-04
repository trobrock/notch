// Package pricing estimates API-list-price costs when a provider does not
// report a request cost. Rates are versioned so persisted historical estimates
// remain attributable to the table used at the time.
package pricing

import (
	"strings"

	"github.com/trobrock/notch/internal/model"
)

const Version = "builtin-2026-05-22"

type Rates struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

type tier struct {
	InputTokensAbove int
	Rates            Rates
}

type entry struct {
	Rates Rates
	Tier  *tier
}

var catalog = map[string]map[string]entry{
	"anthropic": {
		"claude-fable-5-1":  {Rates: Rates{Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5}},
		"claude-haiku-4-5":  {Rates: Rates{Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25}},
		"claude-opus-4-6":   {Rates: Rates{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25}},
		"claude-opus-4-7":   {Rates: Rates{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25}},
		"claude-opus-4-8":   {Rates: Rates{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25}},
		"claude-opus-5":     {Rates: Rates{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25}},
		"claude-sonnet-4-5": {Rates: Rates{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}},
		"claude-sonnet-4-6": {Rates: Rates{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}},
		"claude-sonnet-5":   {Rates: Rates{Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5}},
	},
	"openai": {
		"gpt-5":               {Rates: Rates{Input: 1.25, Output: 10, CacheRead: 0.125}},
		"gpt-5-mini":          {Rates: Rates{Input: 0.25, Output: 2, CacheRead: 0.025}},
		"gpt-5.3-codex-spark": {Rates: Rates{Input: 1.75, Output: 14, CacheRead: 0.175}},
		"gpt-5.4":             tiered(Rates{Input: 2.5, Output: 15, CacheRead: 0.25}, Rates{Input: 5, Output: 22.5, CacheRead: 0.5}),
		"gpt-5.4-mini":        {Rates: Rates{Input: 0.75, Output: 4.5, CacheRead: 0.075}},
		"gpt-5.5":             tiered(Rates{Input: 5, Output: 30, CacheRead: 0.5}, Rates{Input: 10, Output: 45, CacheRead: 1}),
		"gpt-5.6-luna":        tiered(Rates{Input: 0.2, Output: 1.2, CacheRead: 0.02, CacheWrite: 0.25}, Rates{Input: 0.4, Output: 1.8, CacheRead: 0.04, CacheWrite: 0.5}),
		"gpt-5.6-sol":         tiered(Rates{Input: 5, Output: 30, CacheRead: 0.5, CacheWrite: 6.25}, Rates{Input: 10, Output: 45, CacheRead: 1, CacheWrite: 12.5}),
		"gpt-5.6-terra":       tiered(Rates{Input: 2, Output: 12, CacheRead: 0.2, CacheWrite: 2.5}, Rates{Input: 4, Output: 18, CacheRead: 0.4, CacheWrite: 5}),
	},
}

func tiered(base, high Rates) entry {
	return entry{Rates: base, Tier: &tier{InputTokensAbove: 272000, Rates: high}}
}

// Estimate returns API-list-price cost in USD for a provider response. The
// boolean is false when the model is not in the versioned pricing catalog.
func Estimate(provider, modelID, cacheRetention string, usage model.Response) (float64, bool) {
	family := providerFamily(provider)
	models := catalog[family]
	value, ok := lookup(models, modelID)
	if !ok {
		return 0, false
	}
	rates := value.Rates
	if value.Tier != nil && usage.TotalInputTokens() > value.Tier.InputTokensAbove {
		rates = value.Tier.Rates
	}
	cacheWriteRate := rates.CacheWrite
	if family == "anthropic" && cacheRetention == "long" {
		cacheWriteRate = rates.Input * 2
	}
	cost := float64(usage.InputTokens)*rates.Input +
		float64(usage.OutputTokens)*rates.Output +
		float64(usage.CacheReadTokens)*rates.CacheRead +
		float64(usage.CacheWriteTokens)*cacheWriteRate
	return cost / 1_000_000, true
}

func providerFamily(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "anthropic-claude-code":
		return "anthropic"
	case "openai", "openai-codex":
		return "openai"
	default:
		return ""
	}
}

func lookup(values map[string]entry, modelID string) (entry, bool) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if value, ok := values[id]; ok {
		return value, true
	}
	// Dated provider snapshots retain the base model's pricing. Avoid matching
	// arbitrary variants such as -mini by requiring a date-like suffix.
	for base, value := range values {
		if strings.HasPrefix(id, base+"-20") {
			return value, true
		}
	}
	return entry{}, false
}
