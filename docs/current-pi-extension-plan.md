# Current Pi extension migration plan

This maps the Pi installation reviewed before Notch was created. It is a porting plan, not a claim that the TypeScript files run unchanged.

| Pi extension | Notch target | Current status |
|---|---|---|
| `developerly-ask-user-question.ts` | Official Lua tool using `notch.ui.input/select` | Host primitives exist; port needed |
| `developerly-auto-trust-worktrees.ts` | Native persisted workspace trust | Implemented with repository-wide trust shared through the canonical Git common directory; interactive prompt only when the active worktree has project inputs, plus `--trust-workspace` and `--safe` |
| `developerly-explore-subagent.ts` | Official executable plugin spawning an isolated Notch child | Plugin/process primitives exist; hardened runner and port needed |
| `developerly-goal-loop.ts` | Executable plugin using lifecycle hooks | Basic hooks exist; follow-up/session host APIs need expansion |
| `developerly-model-system-prompt.ts` | Lua `before_agent_start` hook | Supported; port needed |
| `developerly-monitor.ts` | Executable plugin plus supervised background-task service | Plugins exist; durable supervisor/follow-up API not implemented |
| `developerly-plan-mode.ts` | Native/Lua policy using tool interception | Denial and argument mutation exist; active-tool sets and shortcuts do not |
| `developerly-session-notes.ts` | External Lua package using durable custom session entries, prompt-editor access, and `session_change` | Host APIs implemented; package lives in [`trobrock/notch-notes`](https://github.com/trobrock/notch-notes) |
| `developerly-status.ts` | Lua lifecycle hooks | Supported; tmux status publication can use `notch.exec` |
| `developerly-subagent.ts` | Official executable plugin spawning isolated Notch children | Plugin/process primitives exist; concurrency/depth policy and port needed |
| `developerly-task-list-tracker.ts` | Official tool plugin plus a generic core task-widget host API | Tool can be ported; the task-widget host primitive does not exist |
| `developerly-token-efficiency.ts` | Native context/compaction middleware | Native compaction exists, but extension context replacement/compaction hooks do not |
| `@benvargas/pi-firecrawl` | Native HTTP tool or MCP server | Native MCP tools work; direct Firecrawl adapter not bundled |
| `pi-mcp-adapter` | Notch's native MCP client | Stdio, Streamable HTTP tools, and authorization-code OAuth work; elicitation, sampling, apps, and scripting remain |

## Core work required for full parity

1. Harden official subagent plugins around isolated `notch --mode rpc --no-session --tools ...` children with model/tool/cwd selection, structured events, usage, cancellation, depth guards, and concurrency limits. Promote child execution to a core host service only if the subprocess boundary proves insufficient.
2. Add supervised background jobs that survive individual turns and can enqueue a follow-up.
3. Add context replacement and compaction hooks.
4. Add active-tool sets and configurable keybindings; the native persisted workspace trust gate now runs before project inputs are loaded.
5. Add a narrow declarative task-widget host API first, then broader widgets, editor dialogs, and session-tree events only when another feature needs them.
6. Add branch-aware custom session entries if Notch later gains session trees; flat current-session custom entries and prompt-editor access are now supported.
7. Add MCP OAuth, elicitation, sampling, resources/prompts, and app UI support.

The current MVP proves the deployment and extension boundaries without requiring Node or npm. Full behavior parity requires the host services above rather than merely translating TypeScript into Lua.
