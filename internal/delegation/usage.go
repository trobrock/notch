package delegation

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
)

// Usage records aggregate work delegated to child Notch agents.
type Usage struct {
	Turns            int      `json:"turns"`
	InputTokens      int      `json:"input_tokens"`
	OutputTokens     int      `json:"output_tokens"`
	CacheReadTokens  int      `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int      `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int      `json:"reasoning_tokens,omitempty"`
	CostUSD          *float64 `json:"cost_usd,omitempty"`
	WallMS           int64    `json:"wall_ms"`
	Calls            int      `json:"calls"`
}

func (u Usage) TotalTokens() int {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

func (u Usage) Empty() bool {
	return u.Turns == 0 && u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 && u.ReasoningTokens == 0 && u.CostUSD == nil && u.WallMS == 0 && u.Calls == 0
}

func (u Usage) Add(other Usage) Usage {
	hadUsage := !u.Empty()
	u.Turns += other.Turns
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.ReasoningTokens += other.ReasoningTokens
	if !hadUsage {
		u.CostUSD = cloneCost(other.CostUSD)
	} else if u.CostUSD != nil && other.CostUSD != nil {
		cost := *u.CostUSD + *other.CostUSD
		u.CostUSD = &cost
	} else {
		u.CostUSD = nil
	}
	u.WallMS += other.WallMS
	u.Calls += other.Calls
	return u
}

func cloneCost(cost *float64) *float64 {
	if cost == nil {
		return nil
	}
	value := *cost
	return &value
}

func (u Usage) Validate() error {
	if u.Turns < 0 || u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 || u.ReasoningTokens < 0 || u.WallMS < 0 || u.Calls < 0 {
		return fmt.Errorf("delegated usage cannot be negative")
	}
	if u.CostUSD != nil && (*u.CostUSD < 0 || math.IsNaN(*u.CostUSD) || math.IsInf(*u.CostUSD, 0)) {
		return fmt.Errorf("delegated cost must be a finite non-negative number")
	}
	return nil
}

func FromDetails(details map[string]any) (Usage, bool) {
	if len(details) == 0 {
		return Usage{}, false
	}
	value, ok := details["delegated_usage"]
	if !ok || value == nil {
		return Usage{}, false
	}
	switch typed := value.(type) {
	case Usage:
		return typed, typed.Validate() == nil
	case *Usage:
		if typed == nil {
			return Usage{}, false
		}
		return *typed, typed.Validate() == nil
	case map[string]any:
		parsed, ok := fromMap(typed)
		return parsed, ok && parsed.Validate() == nil
	default:
		// Extension/plugin details often cross a JSON boundary and may arrive
		// as map aliases rather than map[string]any. Decode any map or struct
		// with the delegated usage JSON shape without coupling the agent to a
		// specific extension implementation.
		kind := reflect.TypeOf(value)
		if kind == nil || (kind.Kind() != reflect.Map && kind.Kind() != reflect.Struct && kind.Kind() != reflect.Ptr) {
			return Usage{}, false
		}
		data, err := json.Marshal(value)
		if err != nil {
			return Usage{}, false
		}
		var parsed Usage
		if json.Unmarshal(data, &parsed) != nil || parsed.Validate() != nil {
			return Usage{}, false
		}
		return parsed, true
	}
}

func fromMap(value map[string]any) (Usage, bool) {
	turns, ok := numberAsInt(value["turns"])
	if !ok {
		return Usage{}, false
	}
	input, ok := numberAsInt(value["input_tokens"])
	if !ok {
		return Usage{}, false
	}
	output, ok := numberAsInt(value["output_tokens"])
	if !ok {
		return Usage{}, false
	}
	cacheRead, ok := optionalInt(value, "cache_read_tokens")
	if !ok {
		return Usage{}, false
	}
	cacheWrite, ok := optionalInt(value, "cache_write_tokens")
	if !ok {
		return Usage{}, false
	}
	reasoning, ok := optionalInt(value, "reasoning_tokens")
	if !ok {
		return Usage{}, false
	}
	cost, ok := optionalFloat(value, "cost_usd")
	if !ok {
		return Usage{}, false
	}
	wall, ok := numberAsInt64(value["wall_ms"])
	if !ok {
		return Usage{}, false
	}
	calls, ok := numberAsInt(value["calls"])
	if !ok {
		return Usage{}, false
	}
	return Usage{
		Turns: turns, InputTokens: input, OutputTokens: output,
		CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite, ReasoningTokens: reasoning, CostUSD: cost,
		WallMS: wall, Calls: calls,
	}, true
}

func optionalInt(value map[string]any, key string) (int, bool) {
	raw, exists := value[key]
	if !exists || raw == nil {
		return 0, true
	}
	return numberAsInt(raw)
}

func optionalFloat(value map[string]any, key string) (*float64, bool) {
	raw, exists := value[key]
	if !exists || raw == nil {
		return nil, true
	}
	var result float64
	switch typed := raw.(type) {
	case float64:
		result = typed
	case float32:
		result = float64(typed)
	case int:
		result = float64(typed)
	case int64:
		result = float64(typed)
	default:
		return nil, false
	}
	return &result, true
}

func numberAsInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int8:
		return int(typed), true
	case int16:
		return int(typed), true
	case int32:
		return int(typed), true
	case int64:
		if typed < math.MinInt || typed > math.MaxInt {
			return 0, false
		}
		return int(typed), true
	case float32:
		if math.Trunc(float64(typed)) != float64(typed) {
			return 0, false
		}
		return int(typed), true
	case float64:
		if math.Trunc(typed) != typed || typed < math.MinInt || typed > math.MaxInt {
			return 0, false
		}
		return int(typed), true
	default:
		return 0, false
	}
}

func numberAsInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case float32:
		if math.Trunc(float64(typed)) != float64(typed) {
			return 0, false
		}
		return int64(typed), true
	case float64:
		if math.Trunc(typed) != typed || typed < math.MinInt64 || typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	default:
		return 0, false
	}
}
