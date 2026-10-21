---
name: software-team
description: Autonomously deliver a software objective with measurable outcomes, choosing direct work or bounded specialist helpers as useful.
---

# Software delivery workflow

Use this workflow when the user or a Developerly handoff provides a software objective and measurable outcomes. Keep one lead. The lead owns scope, architecture, integration, final checks, and completion evidence.

Developerly is the source of truth for the work item. Do not create a second project plan or duplicate its status. Keep the Developerly work-item ID in the result when one is provided.

## Start from outcomes

1. Read the objective, measurable outcomes, constraints, and requested Git result.
2. If the handoff explicitly asks for bounded goal mode, call `start_goal` with the objective and the outcomes as exact completion checks. Do not add vague checks.
3. Inspect the repository and its guidance before choosing an approach.
4. Make the smallest coherent plan. Ask a question only when a missing answer truly blocks safe work.

A good Developerly handoff looks like:

```text
Use goal mode to complete Developerly work item 123.

Objective: Add CSV export to filtered reports.

Completion checks:
- Export uses the active filters.
- Exported values match the visible table.
- Focused tests pass.
- Required repository checks pass.

Choose direct work or specialist helpers based on expected value. Keep one lead responsible for integration and evidence.
```

## Choose tools intelligently

Direct execution is the default for small, tightly coupled work. Do not create a team merely because roles exist.

Use helpers when they reduce uncertainty or elapsed time:

- **Researcher:** Trace a broad or unfamiliar code path. Read-only.
- **Designer:** Define user flow, states, accessibility, and visual behavior for meaningful UI work. Usually read-only.
- **Developer (`implementer` alias):** Make one narrow, separable change in an existing clean worktree.
- **QA:** Run focused checks and boundary cases. Usually read-only.
- **Reviewer:** Independently inspect the integrated result. Read-only.

Use `explore_codebase` for broad read-only discovery. Use `run_subagent` for one focused helper. Use `coordinate_helpers` only for two or more independent tasks whose parallel value exceeds startup and integration cost. Do not require every role.

Use the global role contracts and the active profile's `role_models`. Never switch providers silently. A role changes instructions and model defaults; it does not change tools.

## Writing helpers

Helpers are read-only by default. Setting `allowWriteTools=true` or `allow_write_tools=true` authorizes that helper invocation to use the requested write-capable tools. Do not ask for a second permission after choosing that flag.

For coordinated change helpers, preserve the existing Git safety rules:

- use a separate clean worktree and named branch;
- start at the exact expected lead commit;
- set `integration_owner=lead`;
- never let a helper merge, reset, clean, or delete a worktree;
- inspect the helper result and integrate explicitly.

Prefer lead-owned edits when creating and integrating a writer worktree costs more than the change itself.

## Delivery loop

1. Investigate only enough to choose a sound implementation.
2. Record concrete progress with `report_goal_progress` when goal mode is active.
3. Implement directly or delegate narrow work.
4. Inspect all helper evidence. A helper success flag is not proof.
5. Integrate writer changes explicitly and resolve conflicts in the lead tree.
6. Run focused tests, then required repository checks.
7. Use QA or an independent reviewer when risk warrants it.
8. Fix material findings and rerun affected checks.
9. Call `complete_goal` only with evidence for every configured check.
10. Return a concise result suitable for updating the Developerly work item: changes, evidence, Git state, usage when known, and unresolved risk.

If blocked, explain exactly what is missing. Do not pause the goal yourself and do not claim completion.

## Budgets and stopping

Do not impose small turn, total-runtime, or cost caps by default. Helpers may continue while making useful progress; the runner stops a helper after 30 minutes without provider or tool events. Use explicit caps when the user requests them or the task has a real deadline. Keep reports concise, but allow enough work to finish and verify the task.

Prefer soft progress checkpoints over restarting a productive helper. Reconsider repeated failed approaches, duplicate work, or reviews that find no new evidence. Increase model capability when evidence shows it is needed, not merely because an arbitrary turn count was reached. Report known usage and costs without treating them as mandatory ceilings.

Human cancellation has priority. Do not retry an interrupted write unless the user asks. Stop duplicate research and repeated review passes.

## Completion contract

Before completion, provide evidence for:

- every measurable outcome;
- required tests, build, and lint checks;
- resolved material review findings;
- the requested Git result;
- deployment and rollback state only when deployment was requested;
- remaining uncertainty.

Never claim a deployment, review, test, clean tree, or improvement over a single-agent baseline without direct evidence.
