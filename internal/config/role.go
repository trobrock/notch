package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// RoleResolution is a frozen, non-secret record of the role used for one
// delegation. Keeping it with a result makes later config drift visible.
type RoleResolution struct {
	Name            string  `json:"name"`
	RequestedName   string  `json:"requested_name,omitempty"`
	Source          string  `json:"source"`
	Instructions    string  `json:"instructions"`
	Model           string  `json:"model"`
	Thinking        string  `json:"thinking"`
	TimeoutSeconds  int     `json:"timeout_seconds,omitempty"`
	MaxOutputChars  int     `json:"max_output_chars,omitempty"`
	MaxTurns        int     `json:"max_turns,omitempty"`
	MaxCostUSD      float64 `json:"max_cost_usd,omitempty"`
	Profile         string  `json:"profile,omitempty"`
	DefinitionHash  string  `json:"definition_hash"`
	ModelProvenance string  `json:"model_provenance"`
}

var starterRoles = map[string]RoleConfig{
	"lead": {
		Instructions: "Own scope, decisions, integration, and completion evidence. Delegate only bounded work and verify important claims.",
		Thinking:     "high", TimeoutSeconds: 600, MaxOutputChars: 16000, MaxTurns: 12,
	},
	"researcher": {
		Instructions: "Investigate the scoped question read-only. Cite exact files and distinguish verified facts from hypotheses.",
		Thinking:     "low", TimeoutSeconds: 300, MaxOutputChars: 12000, MaxTurns: 8,
	},
	"designer": {
		Instructions: "Design the smallest coherent solution. State tradeoffs, boundaries, risks, and clear acceptance checks. Do not implement unless the task explicitly asks for it and write access is separately granted.",
		Thinking:     "high", TimeoutSeconds: 300, MaxOutputChars: 12000, MaxTurns: 8,
	},
	"developer": {
		Instructions: "Implement only the approved scope. Preserve unrelated changes, add focused tests, and report exact files and checks.",
		Thinking:     "medium", TimeoutSeconds: 600, MaxOutputChars: 16000, MaxTurns: 12,
	},
	"qa": {
		Instructions: "Test the scoped behavior against its acceptance checks. Report commands, evidence, regressions, and remaining uncertainty. Do not fix issues unless separately asked and authorized.",
		Thinking:     "medium", TimeoutSeconds: 300, MaxOutputChars: 12000, MaxTurns: 8,
	},
	"reviewer": {
		Instructions: "Review independently for correctness, security, regressions, and missing tests. Lead with actionable findings and cite exact locations.",
		Thinking:     "high", TimeoutSeconds: 300, MaxOutputChars: 12000, MaxTurns: 8,
	},
}

// CanonicalRoleName preserves the old implementer spelling as an alias for the
// canonical developer role.
func CanonicalRoleName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "implementer" {
		return "developer"
	}
	return name
}

func normalizeRoleModels(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for name, model := range values {
		name, model = CanonicalRoleName(name), strings.TrimSpace(model)
		if name != "" && model != "" {
			out[name] = model
		}
	}
	return out
}

// ResolveRole resolves built-in or global custom role policy. Explicit values
// are applied by delegation tools after this result, so caller limits and tool
// authorization stay separate.
func ResolveRole(cfg Config, routing *RoutingSnapshot, requested string) (RoleResolution, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	name := CanonicalRoleName(requested)
	if name == "" {
		return RoleResolution{}, nil
	}
	role, ok := starterRoles[name]
	source := "built-in"
	if custom, exists := cfg.Roles[name]; exists {
		role, ok, source = custom, true, "user config"
	} else if requested == "implementer" {
		if custom, exists := cfg.Roles[requested]; exists {
			role, ok, source = custom, true, "user config alias"
		}
	}
	if !ok {
		names := make([]string, 0, len(starterRoles)+len(cfg.Roles))
		seen := map[string]bool{}
		for candidate := range starterRoles {
			seen[candidate] = true
			names = append(names, candidate)
		}
		for candidate := range cfg.Roles {
			candidate = CanonicalRoleName(candidate)
			if candidate != "" && !seen[candidate] {
				seen[candidate] = true
				names = append(names, candidate)
			}
		}
		sort.Strings(names)
		return RoleResolution{}, fmt.Errorf("unknown role %q; available roles: %s (implementer is an alias for developer)", requested, strings.Join(names, ", "))
	}
	role.Instructions = strings.TrimSpace(role.Instructions)
	role.Model = strings.TrimSpace(role.Model)
	role.Thinking = strings.ToLower(strings.TrimSpace(role.Thinking))
	if role.Instructions == "" {
		return RoleResolution{}, fmt.Errorf("role %q: instructions are required", requested)
	}
	if role.Thinking != "" && !validProfileThinking(role.Thinking) {
		return RoleResolution{}, fmt.Errorf("role %q: invalid thinking %q", requested, role.Thinking)
	}
	if role.TimeoutSeconds < 0 || role.TimeoutSeconds > 3600 || role.MaxOutputChars < 0 || role.MaxOutputChars > 50000 || (role.MaxOutputChars > 0 && role.MaxOutputChars < 1000) || role.MaxTurns < 0 || role.MaxCostUSD < 0 || math.IsNaN(role.MaxCostUSD) || math.IsInf(role.MaxCostUSD, 0) {
		return RoleResolution{}, fmt.Errorf("role %q: bounded defaults are invalid (timeout <= 3600, output 1000..50000 when set, and other limits finite and non-negative)", requested)
	}
	resolved := RoleResolution{Name: name, RequestedName: requested, Source: source, Instructions: role.Instructions, Model: role.Model, Thinking: role.Thinking, TimeoutSeconds: role.TimeoutSeconds, MaxOutputChars: role.MaxOutputChars, MaxTurns: role.MaxTurns, MaxCostUSD: role.MaxCostUSD, ModelProvenance: "role default"}
	if routing != nil {
		resolved.Profile = routing.Profile
		if mapped := strings.TrimSpace(routing.RoleModels[name]); mapped != "" {
			resolved.Model, resolved.ModelProvenance = mapped, "profile role mapping"
		}
	}
	if resolved.Model == "" {
		resolved.ModelProvenance = "base delegation default"
	}
	RefreshRoleHash(&resolved)
	return resolved, nil
}

// RefreshRoleHash seals the current effective non-secret role record.
func RefreshRoleHash(resolved *RoleResolution) {
	if resolved == nil {
		return
	}
	resolved.DefinitionHash = ""
	canonical, _ := json.Marshal(resolved)
	sum := sha256.Sum256(canonical)
	resolved.DefinitionHash = hex.EncodeToString(sum[:])
}
