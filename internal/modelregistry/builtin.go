package modelregistry

var anthropicCatalog = []Entry{
	{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5", ContextWindow: 200000, Reasoning: true},
	{ID: "claude-opus-4-6", Name: "Claude Opus 4.6", ContextWindow: 1000000, Reasoning: true},
	{ID: "claude-opus-4-7", Name: "Claude Opus 4.7", ContextWindow: 1000000, Reasoning: true},
	{ID: "claude-opus-4-8", Name: "Claude Opus 4.8", ContextWindow: 1000000, Reasoning: true},
	{ID: "claude-opus-5", Name: "Claude Opus 5", ContextWindow: 1000000, Reasoning: true},
	{ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5", ContextWindow: 1000000, Reasoning: true},
	{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", ContextWindow: 1000000, Reasoning: true},
	{ID: "claude-fable-5-1", Name: "Claude Fable 5.1", ContextWindow: 1000000, Reasoning: true},
	{ID: "claude-sonnet-5", Name: "Claude Sonnet 5", ContextWindow: 1000000, Reasoning: true},
}

// Builtin returns the small offline fallback shipped with Notch. Provider APIs
// replace this list once a successful discovery has been cached.
func Builtin(provider string) []Entry {
	catalog := map[string][]Entry{
		"openai-codex": {
			{ID: "gpt-5.3-codex-spark", Name: "GPT-5.3 Codex Spark", ContextWindow: 128000, Reasoning: true},
			{ID: "gpt-5.4", Name: "GPT-5.4", ContextWindow: 272000, Reasoning: true},
			{ID: "gpt-5.4-mini", Name: "GPT-5.4 mini", ContextWindow: 272000, Reasoning: true},
			{ID: "gpt-5.5", Name: "GPT-5.5", ContextWindow: 272000, Reasoning: true},
			{ID: "gpt-5.6-luna", Name: "GPT-5.6 Luna", ContextWindow: 272000, Reasoning: true},
			{ID: "gpt-5.6-sol", Name: "GPT-5.6 Sol", ContextWindow: 272000, Reasoning: true},
			{ID: "gpt-5.6-terra", Name: "GPT-5.6 Terra", ContextWindow: 272000, Reasoning: true},
		},
		"anthropic":             cloneEntries(anthropicCatalog),
		"anthropic-claude-code": cloneEntries(anthropicCatalog),
		"openrouter": {
			{ID: "anthropic/claude-sonnet-4.5", Name: "Claude Sonnet 4.5", ContextWindow: 1000000, Reasoning: true},
			{ID: "openai/gpt-5", Name: "OpenAI GPT-5", ContextWindow: 400000, Reasoning: true},
		},
		"openai": {
			{ID: "gpt-5", Name: "GPT-5", ContextWindow: 400000, Reasoning: true},
			{ID: "gpt-5-mini", Name: "GPT-5 mini", ContextWindow: 400000, Reasoning: true},
		},
	}
	values := cloneEntries(catalog[provider])
	for i := range values {
		values[i].Provider = provider
		values[i].Source = "bundled"
	}
	return values
}
