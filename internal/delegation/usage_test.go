package delegation

import (
	"encoding/json"
	"testing"
)

func TestFromDetailsAcceptsTypedAndJSONShapes(t *testing.T) {
	want := Usage{Turns: 2, InputTokens: 7, OutputTokens: 3, WallMS: 40, Calls: 1}
	for name, details := range map[string]map[string]any{
		"typed": {"delegated_usage": want},
		"map": {"delegated_usage": map[string]any{
			"turns": 2, "input_tokens": 7, "output_tokens": 3, "wall_ms": 40, "calls": 1,
		}},
		"json numbers": func() map[string]any {
			var value map[string]any
			if err := json.Unmarshal([]byte(`{"delegated_usage":{"turns":2,"input_tokens":7,"output_tokens":3,"wall_ms":40,"calls":1}}`), &value); err != nil {
				t.Fatal(err)
			}
			return value
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := FromDetails(details)
			if !ok || got != want {
				t.Fatalf("FromDetails() = %#v, %v", got, ok)
			}
		})
	}
}

func TestUsageAddIncludesCachedTokensAndCompleteCost(t *testing.T) {
	firstCost, secondCost := 0.002, 0.003
	got := (Usage{}).Add(Usage{
		Turns: 1, InputTokens: 7, OutputTokens: 3, CacheReadTokens: 5,
		CacheWriteTokens: 2, ReasoningTokens: 1, CostUSD: &firstCost, Calls: 1,
	}).Add(Usage{
		Turns: 1, InputTokens: 4, OutputTokens: 2, CacheReadTokens: 3,
		ReasoningTokens: 1, CostUSD: &secondCost, Calls: 1,
	})
	if got.TotalTokens() != 26 || got.CacheReadTokens != 8 || got.CacheWriteTokens != 2 || got.ReasoningTokens != 2 || got.CostUSD == nil || *got.CostUSD != 0.005 {
		t.Fatalf("Add() = %#v", got)
	}

	got = got.Add(Usage{Turns: 1, InputTokens: 1, OutputTokens: 1, Calls: 1})
	if got.CostUSD != nil {
		t.Fatalf("incomplete aggregate cost must be unknown: %#v", got)
	}
}

func TestFromDetailsAcceptsExtendedUsage(t *testing.T) {
	got, ok := FromDetails(map[string]any{"delegated_usage": map[string]any{
		"turns": 1, "input_tokens": 7, "output_tokens": 3,
		"cache_read_tokens": 5, "cache_write_tokens": 2, "reasoning_tokens": 1,
		"cost_usd": 0.004, "wall_ms": 40, "calls": 1,
	}})
	if !ok || got.CacheReadTokens != 5 || got.CacheWriteTokens != 2 || got.ReasoningTokens != 1 || got.CostUSD == nil || *got.CostUSD != 0.004 {
		t.Fatalf("FromDetails() = %#v, %v", got, ok)
	}
}

func TestFromDetailsRejectsInvalidUsage(t *testing.T) {
	for _, value := range []any{
		map[string]any{"turns": -1, "input_tokens": 0, "output_tokens": 0, "wall_ms": 0, "calls": 1},
		map[string]any{"turns": 1, "input_tokens": 1.5, "output_tokens": 0, "wall_ms": 0, "calls": 1},
		map[string]any{"turns": 1},
	} {
		if got, ok := FromDetails(map[string]any{"delegated_usage": value}); ok {
			t.Fatalf("accepted invalid usage: %#v", got)
		}
	}
}
