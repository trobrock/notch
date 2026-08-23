# Current Pi extension migration plan

This maps the Pi installation reviewed before Notch was created. It is a porting plan, not a claim that the TypeScript files run unchanged.

| Pi extension | Notch target | Current status |
|---|---|---|
| `developerly-ask-user-question.ts` | Lua tool using `notch.ui.input/select` | Host primitives exist; port needed |
| `developerly-auto-trust-worktrees.ts` | Early native trust policy | Trust phase/store not implemented |
| `developerly-explore-subagent.ts` | Native subagent service plus thin tool | Subagent service not implemented |
| `developerly-goal-loop.ts` | Executable plugin using lifecycle hooks | Basic hooks exist; follow-up/session host APIs need expansion |
| `developerly-model-system-prompt.ts` | Lua `before_agent_start` hook | Supported; port needed |
| `developerly-monitor.ts` | Executable plugin plus supervised background-task service | Plugins exist; durable supervisor/follow-up API not implemented |
| `developerly-plan-mode.ts` | Native/Lua policy using tool interception | Denial and argument mutation exist; active-tool sets and shortcuts do not |
| `developerly-session-notes.ts` | Lua command plus custom session entries | Commands exist; session-entry/UI editor host APIs do not |
| `developerly-status.ts` | Lua lifecycle hooks | Supported; tmux status publication can use `notch.exec` |
| `developerly-subagent.ts` | Native subagent service plus thin tool | Subagent service not implemented |
| `developerly-task-list-tracker.ts` | Native tool and declarative widget | Tool can be ported; widgets/session-tree events do not exist |
| `developerly-token-efficiency.ts` | Native context/compaction middleware | Prompt hook exists; context replacement and compaction do not |
| `@benvargas/pi-firecrawl` | Native HTTP tool or MCP server | Native MCP tools work; direct Firecrawl adapter not bundled |
| `pi-mcp-adapter` | Notch's native MCP client | Stdio and Streamable HTTP tools work; OAuth, elicitation, sampling, apps, and scripting remain |

## Core work required for full parity

1. Add a first-class subagent service with model/tool/cwd selection, structured events, usage, cancellation, and concurrency limits.
2. Add supervised background jobs that survive individual turns and can enqueue a follow-up.
3. Add context replacement and compaction hooks.
4. Add active-tool sets, keybindings, and a trust hook that runs before project extensions.
5. Add declarative widgets, editor dialogs, and session-tree events.
6. Add custom session entries to the executable and Lua host APIs.
7. Add MCP OAuth, elicitation, sampling, resources/prompts, and app UI support.

The current MVP proves the deployment and extension boundaries without requiring Node or npm. Full behavior parity requires the host services above rather than merely translating TypeScript into Lua.
