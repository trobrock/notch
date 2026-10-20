# Reusable specialist roles

Notch can apply one reusable role contract to the existing delegation tools. A role adds task instructions and bounded defaults. It does **not** grant tools, shell or network access, write-capable tools, credentials, or a worktree.

Built-in roles are `lead`, `researcher`, `designer`, `developer`, `qa`, and `reviewer`. `implementer` is a supported alias for `developer`, so older team calls keep working.

## Configure roles globally

Roles and profile role mappings are global-only. Put them in the user config, not `.notch/config.json`:

```json
{
  "roles": {
    "auditor": {
      "instructions": "Check the requested policy and cite exact evidence.",
      "thinking": "high",
      "timeout_seconds": 300,
      "max_output_chars": 12000,
      "max_turns": 8,
      "max_cost_usd": 0.75
    }
  },
  "profiles": {
    "personal": {
      "provider": "openai-codex",
      "model": "FULL_MODEL_ID",
      "allowed_providers": ["openai-codex"],
      "role_models": {
        "researcher": "SMALL_MODEL_ID",
        "reviewer": "FULL_MODEL_ID",
        "auditor": "FULL_MODEL_ID"
      }
    },
    "work": {
      "provider": "anthropic-claude-code",
      "model": "FULL_WORK_MODEL_ID",
      "allowed_providers": ["anthropic-claude-code"],
      "role_models": {
        "researcher": "SMALL_WORK_MODEL_ID",
        "reviewer": "FULL_WORK_MODEL_ID",
        "auditor": "FULL_WORK_MODEL_ID"
      }
    }
  }
}
```

Use model IDs returned by `list_models`. Notch validates a role-selected model before dispatch. It fails if the model is unavailable or its provider is outside the active profile. It never searches another provider as a fallback. Model capability and price change over time, so choose a currently available inexpensive model for bounded routine work and escalate only when evidence shows it is needed.

## Use roles

- `run_subagent`: set `role` to a built-in or custom role.
- `explore_codebase`: set a batch `role`, or a role on one task.
- `coordinate_helpers`: set each task's `role`. Built-in kind rules still apply: a developer/implementer must be a `change` task and a reviewer must be a `review` task. Custom roles do not bypass kind, worktree, or write checks.

Calls without `role` keep the old behavior.

Resolution order is:

1. active profile and base delegation defaults;
2. role defaults;
3. the active profile's `role_models` entry;
4. explicit batch or task overrides.

Parent cost and turn limits, profile provider boundaries, and the tool allowlist always win. A role cannot add a tool. The caller enables write-capable tools for one helper invocation with `allowWriteTools=true` or `allow_write_tools=true`.

## Prompt and handoff order

Children receive a compact handoff, not the parent's conversation. Notch composes instructions in this stable order:

1. trusted child safety and reporting rules;
2. applicable user/project guidance loaded for the child's working directory;
3. reusable role instructions;
4. caller `systemPrompt` additions;
5. the self-contained task prompt.

Role text and caller additions are additive. They cannot replace the trusted child rules. Child resource loading stays disabled; project guidance and MCP/environment scope are only what the isolated child process normally receives through its selected settings and granted tools. A profile is a routing boundary, not a process sandbox.

## Provenance and config drift

Progress names the effective role and selected model. Tool results store the canonical role, requested alias, effective non-secret instructions/defaults/model, source, profile, model provenance, and a definition hash. These details are part of the persisted tool result in a normal append-only session, so editing a role later does not rewrite an old delegation.

A changed global role definition applies to the next delegation. It does not resume or mutate an old child run, and it does not require session acknowledgement. Start a new delegation to use it. A changed profile `role_models` mapping changes the profile definition hash; resuming that profile requires the same explicit profile-change acknowledgement used for other routing changes. Old sessions without role provenance remain readable.

## Bounded research and review walkthrough

A repository walkthrough used one four-task read-only research batch, followed by independent read-only review. Initial review attempts hit 4-turn and 8-turn limits with no report. A narrowed 12-turn review completed and found four candidate issues. Parent verification rejected one as a known pre-existing authorization contract and one because agent-side explore defaults already cover it; it confirmed and fixed stale-provider validation and incomplete effective provenance. Two post-fix reviews then hit their 8-turn and 12-turn limits without a report. A final focused one-task review returned no findings, but direct parent verification still found and fixed two missed defects: explicit explore models lacked override provenance, and role-default turn/cost budgets could reset after a team clarification. In total the walkthrough used seven review attempts: five limit failures, one useful findings report, and one clean-but-incomplete report. Helpers made no writes. Exact token/cost totals were not available from the captured tool output, so cost is recorded as unknown rather than estimated. The result shows why bounded specialists should be compared with a strong single-agent baseline: extra helpers can find defects, but retries, missed defects, and unverified findings add time and rework.
