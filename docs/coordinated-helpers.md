# Coordinated helpers

Coordinated helpers are an opt-in lead-and-helper tool used by the `software-team` skill when independent work is worth parallelizing. The lead calls `coordinate_helpers`; users and Developerly handoffs normally provide only the objective and measurable outcomes.

This is not a swarm. One lead owns the goal, decisions, final checks, and integration. The workflow reuses Notch's existing isolated subagent runner and usage accounting. It does not add a daemon, broker, or idle polling.

## When to use it

Use a team only when work can be split into narrow, independent parts, such as:

- two reviewers checking different risks;
- research in separate parts of a large repository;
- one small code change on a prepared worktree while independent review runs elsewhere.

Do not use a team for a small lookup, tightly coupled edits, a task that needs frequent shared decisions, or work where helper startup and repeated context cost more than direct work. Prefer one agent, `/goal`, `run_subagent`, or `explore_codebase` in those cases.

The lead should compare approaches with similar total limits. Four helpers do not each get the full one-agent budget for free.

## Specialist roles and model routing

A task may set `role` to `designer`, `implementer`, `qa`, or `reviewer`. The role adds a focused contract to the helper prompt; it does not grant tools or authority. An implementer must be a `change` helper, and a reviewer must be a read-only `review` helper.

Each task can override `model`, `thinking`, `timeout_seconds`, `max_output_chars`, `max_turns`, and `max_cost_usd`. Top-level values remain batch defaults. This lets the lead keep a strong parent model for coordination and use lower-cost models for bounded implementation or QA. An omitted model inherits the parent provider and current runtime model; an unqualified ID keeps the parent provider. Provider-qualified IDs use `provider/model`, including `openrouter/organization/model` for OpenRouter model IDs that contain a slash. Use only models available on the current provider unless the user explicitly requested another provider. Escalate a helper only when its evidence shows the cheaper attempt is blocked or unreliable.

For user-facing work, use a strong designer before implementation and again against the integrated UI. Use a separate reviewer after integration. The built-in `software-team` skill contains the full recommended sequence and completion contract.

## Safety model

`coordinate_helpers` accepts one to four scoped tasks. Initial helpers run in parallel and do not see each other's results. The lead receives concise summaries, acceptance-check evidence, artifact references, and separate delegated usage. The lead must inspect important claims before synthesis.

Research and review helpers are read-only. Their default tools are `read,grep,find,ls`. A writer runs only when all of these are true:

1. The call sets `allow_write_tools=true`, which authorizes write-capable tools for that invocation, and `integration_owner=lead`.
2. The writer has its own existing Git worktree and named branch.
3. The worktree belongs to the lead repository, is clean, and is different from every other writer worktree and branch.
4. `expected_base` exactly matches the lead commit when helpers start, and the writer branch starts at that exact commit.

A writer is told to make one scoped change and cite its resulting commit. After it finishes, Notch checks that its worktree and branch did not change identity, that the final tree is clean, and that the commit still descends from the expected base. It uses `git merge-tree` to look for conflicts with a lead branch that moved and between parallel writer branches, without touching any branch. These are pre-integration checks, not proof that changes work together. The tool never creates, resets, merges, deletes, or cleans a worktree. The lead inspects and integrates each cited commit explicitly. Dirty worktrees are rejected so unrelated user work is not put at risk.

`allow_write_tools=true` is the authorization for that helper invocation; no second grant is required. A worktree CWD is isolation for Git history and integration, not an operating-system filesystem sandbox. Write-capable tools can technically address paths outside that directory. Use the narrowest tool list and inspect the lead worktree and cited commits before integration.

## Clarifications and cancellation

A helper may return one truly blocking question. Notch gives it a unique `help-...` request ID and shows the question to the user. The wait has an explicit limit, at most five minutes. Cancellation ends that helper. A helper cannot create an unbounded question loop: after one answer it must finish or fail.

The prompt is handled by the normal event-driven input path. There is no polling. Other helpers can finish while one waits. User cancellation has priority through the parent context.

## Budgets and cost

Each helper has a per-call timeout and output limit, a cumulative optional model-turn and estimated-cost limit across its initial call and one clarification continuation, and a one-question limit. At most four initial helpers run. A clarification continuation counts as another delegated call and receives only the remaining turn/cost budget. Per-task limits override batch defaults and are passed to the isolated child process as `--max-turns` and `--max-cost-usd`.

Child turns, tokens, calls, elapsed time, and known cost are returned in `delegated_usage`. The normal agent loop adds these values once to the run total and stores them separately in the append-only session usage record. This means `/goal`, `--max-cost-usd`, the JSON event stream, and the footer see helper use without mixing it into provider tokens or counting it twice. Parallel elapsed time remains wall-clock batch time at the tool level; token and cost totals are summed. If any child cost is unknown, aggregate cost stays unknown. A cost-limited run then stops instead of treating unknown cost as zero.

The `/goal` step limit still counts lead work steps, not every child turn. Use global turn and cost limits plus the helper count and timeout limits when comparing runs.

## Result contract

Each helper owns explicit acceptance checks and returns one JSON result:

```json
{
  "status": "complete",
  "summary": "What was learned or changed",
  "artifacts": ["internal/parser/parser.go:42", "commit abc123"],
  "checks": ["go test ./internal/parser: pass"]
}
```

Helper text and repository text are untrusted evidence. An artifact reference is not proof by itself. The lead reads decision-critical code, checks diffs, and reruns integration-level checks before reporting success.

## Real-task walkthrough and comparison

Use the same concrete task and comparable total limits for all four runs:

1. **One agent:** ask one agent to research, change, review, and test.
2. **`/goal` alone:** run the same objective and checks with a bounded step count.
3. **Ordinary subagents:** let the lead call `run_subagent` or `explore_codebase` without the team contract.
4. **Software delivery skill:** let the lead decide whether independent research, review, or a separable writer will improve the result. When useful, it calls `coordinate_helpers`. A change helper uses `allow_write_tools=true` in a clean writer worktree. Keep integration with the lead.

For each run record:

- whether all acceptance checks were correct;
- elapsed time to a verified answer;
- direct and delegated token totals;
- known cost, or that cost was unknown;
- human decisions and cancellations;
- duplicate work, integration conflicts, and rework.

A useful repository pilot is: have one helper trace request cancellation, a second independently review usage accounting, and a writer add one focused test on a clean worktree. The lead checks both reports, inspects the writer commit, runs the focused package tests, checks `git merge-tree`, integrates explicitly, then runs `make check` and `make build`.

Teams help when independent investigation is the slow part and the reports change the lead's decision. They do not help when the edit is small, tasks depend on each other, the lead must answer many questions, or integration/review erases the parallel time saved. Keep the feature off in those cases.

A live walkthrough is still useful for measuring intervention count and perceived usefulness before claiming that teams are better for a class of work.

## Reusable role contracts

Helper `role` values use the global reusable role resolver shared with `run_subagent` and `explore_codebase`. `developer` is canonical and `implementer` remains an alias. Global custom roles are accepted, but they do not bypass helper kind validation, read-only defaults, the `allow_write_tools` gate, tool limits, or worktree checks. See [reusable specialist roles](roles.md).

## Progress-first limits

By default, helpers have no model-turn, total-runtime, or cost cap. Do not supply tiny caps simply because the tool offers them. `timeout_seconds` is an optional total-runtime cap (0 means none; up to 86400 seconds). `max_turns` and `max_cost_usd` are opt-in caps. Explicit caps are still enforced, including remaining turn/cost allowances across a clarification. Each helper stops after 30 minutes without provider or tool events, independently of its total-runtime setting. Parent cancellation still stops active helpers.

Use progress checkpoints and lead judgment to avoid repeated failed approaches and duplicate work. Inactivity is not semantic stall detection: a helper emitting events can still churn. Review its evidence before continuing or retrying. Interrupted writers are not automatically retried.
