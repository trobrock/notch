# Personal and work profiles

Profiles make provider routing explicit. They are a routing boundary, not a security sandbox. A profile chooses a provider, model, thinking level, allowed providers, and an optional name for a credential already stored by Notch.

Profiles may be defined only in the user config at `$XDG_CONFIG_HOME/notch/config.json` (normally `~/.config/notch/config.json`). A trusted project `.notch/config.json` cannot define or replace them.

```json
{
  "profiles": {
    "personal": {
      "provider": "anthropic",
      "model": "your-configured-personal-model-id",
      "thinking_level": "medium",
      "allowed_providers": ["anthropic"]
    },
    "work": {
      "provider": "anthropic-claude-code",
      "model": "your-configured-work-model-id",
      "thinking_level": "high",
      "allowed_providers": ["anthropic-claude-code"],
      "credential_ref": "anthropic-claude-code"
    }
  }
}
```

The example model IDs are placeholders. Use IDs returned by `notch models PROVIDER` or supplied by your configured provider. Notch does not guess model IDs.

Use `anthropic` for the Anthropic API-key route. Use `anthropic-claude-code` for the Claude Code OAuth route. They are separate providers and credentials. Pick the one that matches a provider and credential you configured; a model name never causes an implicit switch between them.

Start and inspect a profile. `NOTCH_PROFILE` is useful for choosing a machine's normal profile; an explicit `--profile` takes precedence:

```sh
export NOTCH_PROFILE=personal
notch
notch --profile work --show-effective-settings
```

The fullscreen footer and line-mode startup text show the active profile. RPC `get_state` includes `profile`. `--show-effective-settings` prints only non-secret routing data and exits before provider creation.

## Resolution and enforcement

Notch resolves settings in this order:

1. built-in defaults;
2. user config;
3. trusted project settings;
4. `NOTCH_PROVIDER`, `NOTCH_MODEL`, `NOTCH_EXPLORE_MODEL`, and `NOTCH_THINKING_LEVEL`;
5. the profile selected by `NOTCH_PROFILE`, or by an explicit `--profile` which takes precedence;
6. the selected profile's routing values;
7. explicit CLI `--provider`, `--model`, and `--thinking` overrides.

Every final provider must be in the profile's `allowed_providers`. CLI overrides can narrow or select an allowed route, but never expand the allowlist. Changing providers clears a provider-specific custom endpoint and model defaults are not carried across providers. Unknown profiles, empty profile models, invalid thinking levels, and forbidden providers stop startup with an error. There is no cross-provider fallback.

The same checks apply when a provider is changed in the TUI and before `run_subagent`, `explore_codebase`, or `coordinate_helpers` starts a child. Child processes receive a non-secret resolved routing snapshot. They cannot change that routing by rereading a worktree config or conflicting `NOTCH_*` defaults.

## Credentials and inherited scope

`credential_ref` is a key that must already exist in Notch's credential store. It is not a token. For safety, it must equal the provider name or use `provider:alias`; a reference can never be reused for another provider. `notch login PROVIDER` creates only the canonical provider key, such as `anthropic-claude-code`; Notch does not currently create account aliases. API-key profiles can omit the field and use the provider's existing environment behavior. Profile snapshots, session headers, effective-settings output, and child arguments never contain credential values. If `credential_ref` is omitted, the provider keeps its existing environment/store behavior. An explicit switch to a different allowed provider clears the original provider's credential reference and uses the target provider's normal credential lookup.

Profiles do not isolate operating-system accounts, files, processes, networks, or provider accounts. Children still inherit their working directory, ordinary process environment, enabled resources, and MCP/extension policy as described by their delegation tool flags. `--setting-sources` controls whether a child rereads user/project settings and resources; the inherited routing snapshot still wins. MCP servers can have their own environment and credentials. A profile does not turn them into a sandbox.

## Sessions and resume

New profile sessions store the profile name, effective non-secret route, allowlist, credential reference name, and a hash of the profile definition. Secret values are never stored there.

- `--continue` selects the latest session for the current working directory, regardless of its saved profile.
- Bare `--resume` lists recent sessions for the current directory. Fullscreen `/resume` lists saved sessions. Neither filters by profile.
- Any session, including an older unprofiled session or one whose profile was removed, can resume with the current provider, model, and profile settings.
- Saved routing metadata describes how the session began. It does not lock the session to those settings, and resuming does not rewrite it.
- `--acknowledge-profile-change` is accepted for compatibility but is no longer needed and has no effect.

## Smoke walkthrough

1. Add `personal` and `work` using provider/model IDs that are available in your setup.
2. Run `notch --profile personal --show-effective-settings`. Check the profile, provider, model, and allowlist.
3. Run `notch --profile work --show-effective-settings` and check that the route differs as expected.
4. Start a short saved session under each profile.
5. Run `notch --profile personal --continue`; it must select the latest session in the current directory, regardless of its saved profile.
6. Try an explicit work session with `--profile personal --resume WORK_SESSION_ID`; it must keep the conversation and use the current personal settings.
7. Ask a helper to use a forbidden provider; the delegation tool must reject it before starting the child.

## Specialist model mappings

A global profile may map reusable specialist roles to models with `role_models`. The mapping is part of the profile definition hash stored with new sessions. Resumed sessions use the current mapping without requiring acknowledgement. The mapped provider must remain inside `allowed_providers`, and Notch does not fall back across providers. See [reusable specialist roles](roles.md).
