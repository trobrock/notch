# Coordinated helpers

Coordinated helpers are an opt-in lead-and-helper workflow. Start one with:

```text
/team OBJECTIVE
```

This is not a swarm. One lead owns the goal, decisions, final checks, and integration. The workflow reuses Notch's existing isolated subagent runner and usage accounting. It does not add a daemon, broker, or idle polling.

## When to use it

Use a team only when work can be split into narrow, independent parts, such as:

- two reviewers checking different risks;
- research in separate parts of a large repository;
- one small code change on a prepared worktree while independent review runs elsewhere.

Do not use a team for a small lookup, tightly coupled edits, a task that needs frequent shared decisions, or work where helper startup and repeated context cost more than direct work. Prefer one agent, `/goal`, `run_subagent`, or `explore_codebase` in those cases.

The lead should compare approaches with similar total limits. Four helpers do not each get the full one-agent budget for free.

## Safety model

`coordinate_helpers` accepts one to four scoped tasks. Initial helpers run in parallel and do not see each other's results. The lead receives concise summaries, acceptance-check evidence, artifact references, and separate delegated usage. The lead must inspect important claims before synthesis.

Research and review helpers are read-only. Their default tools are `read,grep,find,ls`. A writer runs only when all of these are true:

1. The user explicitly authorized delegated writes.
2. The call sets `allow_write_tools=true` and `integration_owner=lead`.
3. The writer has its own existing Git worktree and named branch.
4. The worktree belongs to the lead repository, is clean, and is different from every other writer worktree and branch.
5. `expected_base` exactly matches the lead commit when helpers start, and the writer branch starts at that exact commit.

A writer is told to make one scoped change and cite its resulting commit. After it finishes, Notch checks that its worktree and branch did not change identity, that the final tree is clean, and that the commit still descends from the expected base. It uses `git merge-tree` to look for conflicts with a lead branch that moved and between parallel writer branches, without touching any branch. These are pre-integration checks, not proof that changes work together. The tool never creates, resets, merges, deletes, or cleans a worktree. The lead inspects and integrates each cited commit explicitly. Dirty worktrees are rejected so unrelated user work is not put at risk.

Write authorization uses the same trusted tool-call policy as `run_subagent`: the model may set `allow_write_tools=true` only after the user explicitly allows delegated writes. A worktree CWD is isolation for Git history and integration, not an operating-system filesystem sandbox. Authorized tools can technically address paths outside that directory. Use the narrowest tool list, do not authorize a writer for untrusted tasks, and inspect the lead worktree and cited commits before integration.

## Clarifications and cancellation

A helper may return one truly blocking question. Notch gives it a unique `help-...` request ID and shows the question to the user. The wait has an explicit limit, at most five minutes. Cancellation ends that helper. A helper cannot create an unbounded question loop: after one answer it must finish or fail.

The prompt is handled by the normal event-driven input path. There is no polling. Other helpers can finish while one waits. User cancellation has priority through the parent context.

## Budgets and cost

Each helper has a timeout, output limit, and one-question limit. At most four initial helpers run. A clarification continuation counts as another delegated call.

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
4. **`/team`:** assign two independent read-only research/review tasks. If the change is separable, prepare one clean writer worktree and explicitly authorize that one writer. Keep integration with the lead.

For each run record:

- whether all acceptance checks were correct;
- elapsed time to a verified answer;
- direct and delegated token totals;
- known cost, or that cost was unknown;
- human decisions and cancellations;
- duplicate work, integration conflicts, and rework.

A useful repository pilot is: have one helper trace request cancellation, a second independently review usage accounting, and a writer add one focused test on a clean worktree. The lead checks both reports, inspects the writer commit, runs the focused package tests, checks `git merge-tree`, integrates explicitly, then runs `make check` and `make build`.

Teams help when independent investigation is the slow part and the reports change the lead's decision. They do not help when the edit is small, tasks depend on each other, the lead must answer many questions, or integration/review erases the parallel time saved. Keep the feature off in those cases.

The first live walkthrough should be done with the user because authorization, intervention count, and perceived usefulness cannot be established by automated tests alone. Record those measurements before claiming that teams are better for a class of work.
