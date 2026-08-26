package delegation

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
)

// Usage records aggregate work delegated to child Notch agents.
type Usage struct {
	Turns        int   `json:"turns"`
	InputTokens  int   `json:"input_tokens"`
	OutputTokens int   `json:"output_tokens"`
	WallMS       int64 `json:"wall_ms"`
	Calls        int   `json:"calls"`
}

func (u Usage) TotalTokens() int {
	return u.InputTokens + u.OutputTokens
}

func (u Usage) Empty() bool {
	return u.Turns == 0 && u.InputTokens == 0 && u.OutputTokens == 0 && u.WallMS == 0 && u.Calls == 0
}

func (u Usage) Add(other Usage) Usage {
	u.Turns += other.Turns
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.WallMS += other.WallMS
	u.Calls += other.Calls
	return u
}

func (u Usage) Validate() error {
	if u.Turns < 0 || u.InputTokens < 0 || u.OutputTokens < 0 || u.WallMS < 0 || u.Calls < 0 {
		return fmt.Errorf("delegated usage cannot be negative")
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
	wall, ok := numberAsInt64(value["wall_ms"])
	if !ok {
		return Usage{}, false
	}
	calls, ok := numberAsInt(value["calls"])
	if !ok {
		return Usage{}, false
	}
	return Usage{Turns: turns, InputTokens: input, OutputTokens: output, WallMS: wall, Calls: calls}, true
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
