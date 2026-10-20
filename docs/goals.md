# Goal mode

`/goal` is an opt-in mode for a sustained task handled by one Notch agent. It is built into the native Notch binary. It does not start until you ask for it.

## Start and control a goal

The easiest form asks for the missing fields in the composer:

```text
/goal start
```

You can also ask the agent in normal language, such as “start a goal to fix the config parser.” The agent may call `start_goal` only when you explicitly ask for goal mode. It creates completion checks from your requested outcome and uses the normal eight-step default unless you choose another limit.

You can provide everything to the slash command on one line:

```text
/goal start Fix the config parser --check focused parser tests pass --check the user docs explain the syntax --max-steps 8
```

The objective is the text before the first `--check`. Repeat `--check` for each completion check. `--max-steps` may be from 1 to 50 and defaults to 8. `/goal OBJECTIVE ...` is a short form of `/goal start OBJECTIVE ...`.

Commands:

- `/goal` or `/goal status` shows the objective, checks, latest progress, stop reason, cost state, and completion evidence. Status is available while work is running.
- `/goal resume` continues a goal that stopped after restart, interruption, a limit, denied tool, or repeated lack of progress, using the unused part of its step budget.
- `/goal clear` removes the goal from active use and stops before the next model request. Its old snapshots remain in the append-only session file.
- Escape or Ctrl-C aborts active work and pauses the goal.

Human messages always win over an automatic goal continuation at a turn boundary. A queued message is handled first. While the model runs, `/goal status` and `/goal clear` are allowed; starting or resuming another goal is rejected. Escape or Ctrl-C stops active work. Goal mode never polls while idle and never resumes itself after Notch restarts. A saved active goal is restored as stopped, including when user input beats the asynchronous `/resume` UI hook. `/new` gets separate goal state; `/resume` loads the selected session's goal without silently continuing it.

## Completion and stopping

Goal mode adds three model tools:

- `start_goal` starts goal mode from a normal-language request. It is only for explicit user requests, needs an objective and completion checks, and defaults to eight work steps.

- `report_goal_progress` records concrete new progress. Repeating the same summary does not count.
- `complete_goal` first requires recorded progress, then requires non-empty evidence for every exact completion check. Saying “done” in normal assistant text cannot complete a goal. Evidence is model-reported, not an independent verifier, so inspect it with `/goal status`.

When blocked, the model explains exactly what it needs. It cannot pause the goal itself. Escape, Ctrl-C, limits, denied tools, repeated lack of progress, shutdown, and session restore still stop automatic work safely.

The goal pauses when its work-step limit is reached, two work cycles report no new progress, the user aborts, a tool is denied, a run errors, or Notch reaches `--max-turns` or `--max-cost-usd`. A separate hard ceiling of 10 goal model turns per configured work step bounds tool-call loops even when Notch's global `--max-turns` is unlimited; successful completion may use one additional model turn to present its final result, and any tools requested by that response are followed by a stop rather than another model call. Existing delegated usage is included by the normal agent accounting. Cost limits are checked after a model call returns, so one in-flight call can overshoot. If pricing is unknown, status says so; a requested cost ceiling cannot be enforced and the normal run stops.

Goal mode needs session persistence. It is unavailable with `--no-session` because there is nowhere to append durable state. Extension and delegation permissions do not change.

## Design and pilot notes

This is an official Go extension rather than Lua because two small core guarantees were missing: an uncanceled `run_end` hook for abort/error/limit cleanup, and atomic priority between queued human input and hook-generated continuation. The goal policy itself remains isolated in `internal/officialext/goal`.

Focused automated walkthroughs cover a multi-step goal, evidence-gated completion, denied tools, queue priority, aborts, run and hard model-turn limits, repeated no-progress, restart restore, early-input `/resume` isolation, command-to-prompt handoff, and persistence failures. Cost is appended after run end, denied tools halt before another model request, command follow-ups wait for command completion, a hard model-turn ceiling bounds tool loops, and request-time state refresh restores switched sessions without silently restarting them. The model-facing `pause_goal` tool and `/goal pause` command were removed because models used them prematurely instead of continuing useful work or explaining a real blocker.

A same-objective user walkthrough against ordinary single-agent use is still required for product signoff. Suggested comparison:

1. Run an ordinary prompt with an objective and two checks. Record interventions, turns, result, and evidence.
2. Start a new session and run `/goal` with the same text and checks. Inspect `/goal status` after one continuation and at completion.
3. Try a task that needs unavailable credentials. Confirm the model explains the blocker without claiming completion or invoking a pause control.
4. Record any confusing status, wasted turns, or missed checks and fix them before calling the pilot complete.
