---
name: software-team
description: Coordinate an end-to-end software task with a senior lead and bounded designer, implementer, QA, and reviewer agents using role-specific models and budgets.
---

# Software team workflow

Use this workflow only when the user asks for coordinated end-to-end software delivery. Keep one lead coordinator. The lead owns scope, architecture, integration, risk decisions, final checks, and completion evidence.

## Roles

- **Coordinator:** Keep the current strong parent model. Break down work, approve design and architecture, integrate changes, and decide completion. Do not spend coordinator turns on routine isolated work when a bounded helper can do it safely.
- **Designer:** Use a strong model for user flows, information hierarchy, interaction states, responsive behavior, visual direction, and accessibility. Designers are read-only unless the user explicitly authorizes a separate writer task.
- **Implementer:** Use a lower-cost model for narrow approved changes. A writing implementer requires explicit user authorization, a separate clean Git worktree and branch, an exact base commit, and `integration_owner=lead`.
- **QA:** Use a lower-cost model for focused tests, browser checks, boundary cases, and reproducible evidence. Keep it read-only unless test-file changes are explicitly authorized as a writer task.
- **Reviewer:** Use an independent medium or strong model after integration. It does not edit code. It prioritizes correctness, security, maintainability, accessibility, and missing-test findings.

Use only model IDs available from the current provider. If a configured role model is unavailable, list models and choose the closest model in the same family and capability tier. Never switch providers silently.

## Sequence

1. Turn the task into measurable outcome, risk, deployment, and Git-result checks.
2. For user-facing work, ask a designer for a short design brief before implementation.
3. Let the coordinator approve the design and architecture.
4. Delegate narrow independent implementation tasks only when separate worktrees are worth the integration cost.
5. Integrate and inspect every writer commit. Helpers never merge their own work.
6. Run focused checks on the integrated tree.
7. Ask QA to verify user behavior and failure cases.
8. For user-facing work, ask the designer to review the real integrated UI in a browser.
9. Ask an independent reviewer to inspect the final integrated result.
10. Fix valid findings and rerun affected checks.
11. Run repository-required final checks and verify the requested Git result.

## Intercom gates

Use Intercom for long-lived peer agents only. Set `wake=true` only for a bounded request that needs timely work and only when the peer opted into wakeups. Replies never wake agents.

A delivered request is not a completed review. Save the request ID and call `intercom_request_status`. Do not complete a check requiring peer review until status is `replied` and the response has been inspected. If Intercom adds no value over a short-lived helper, use the helper instead.

## Budgets and escalation

Set per-helper `model`, `thinking`, `max_turns`, `max_cost_usd`, `timeout_seconds`, and `max_output_chars`. Start routine implementer and QA tasks with low bounds. Use strong models for design or review judgment. Escalate a helper only when its evidence shows the cheaper attempt is blocked or unreliable; record why.

Stop repeated or duplicate review passes. Any code change after review reruns only the affected checks plus required final checks.

## Completion contract

Before completion, provide evidence for:

- each user-visible acceptance check;
- final integrated tests/build/lint as required by the repository;
- received and resolved review findings, not merely a sent request;
- deployment state and rollback plan when deployment was requested;
- exact Git result requested by the task: validated diff, clean commit, branch, or pull request;
- cost/turn usage by delegated role when available;
- remaining uncertainty.

Never claim a deployment, review, test, or clean Git state without direct evidence.
