# Local intercom plugin

Local intercom is an optional compiled plugin. It lets two running Notch sessions in the **same project** discover each other, send a direct message, and send a reply. It does not create a shared chat, a swarm, or a cloud service.

The plugin is off unless you install it. It uses event-driven Unix domain sockets. It does not poll while idle and adds no Node.js dependency to Notch.

## Build and install

From a Notch source checkout:

```sh
make intercom-plugin
mkdir -p ~/.config/notch/extensions/intercom
cp examples/extensions/intercom/notch-intercom \
  examples/extensions/intercom/plugin.json \
  ~/.config/notch/extensions/intercom/
```

Restart Notch in each terminal. The plugin requires normal durable sessions, so do not use `--no-session`. Run `/intercom status` to check it.

The plugin is trusted local code, like every executable Notch plugin. Review it before installing it.

## Basic use

Give each session a clear name:

```text
/intercom name api
/intercom name tests
```

Find sessions in the same project and send a finding:

```text
/intercom list
/intercom send tests The parser accepts an empty header; please add a regression test.
```

Manual `/intercom send` requests may wake an opted-in receiver. Agent tools are passive by default, but `intercom_send` may explicitly set `wake=true` for a bounded request that needs timely work. The receiver must have wakeups enabled, and each session accepts at most eight automated wakes. Replies never wake peers, which prevents automatic reply loops.

Read stored messages and reply to one request ID:

```text
/intercom inbox
/intercom reply REQUEST_ID Added the regression test in parser_test.go. It fails before the fix and passes now.
```

The model can use `intercom_list`, `intercom_send`, `intercom_inbox`, `intercom_reply`, and `intercom_request_status`. `intercom_send` is passive unless it explicitly sets `wake=true`; opted-in receivers cap automated wakes at eight per session. Replies are always passive. Every confirmed outbound delivery is also appended to the sender's durable session. `intercom_request_status` proves whether a specific recorded request is still pending or has matching replies. Tool access follows the normal Notch tool policy. Intercom does not change delegation permissions or grant new authority.

Messages are limited to 16 KiB, and each durable session accepts at most 1,000 inbound and 1,000 outbound messages. Start a new session if either explicit budget is exhausted. Before transmission, a send is durably recorded with its stable request ID as `pending_delivery`; a confirmed acknowledgement appends delivered state for the same ID. A transport failure is retried once with that ID. If the first acknowledgement was lost, the receiver reports the retry as a duplicate instead of appending or waking twice. Delivery failures include the recorded request ID and remain queryable. A failure while appending confirmed state leaves the durable request conservatively marked pending and returns a warning; it does not generate a second ID.

## Delivery and wakeup modes

Inbound messages are always stored in `intercom_inbound` session entries and shown by `/intercom inbox`. Wakeups are **off by default**.

```text
/intercom wake on
/intercom wake off
```

With wakeups on:

- Manual `/intercom send` requests can wake the receiver.
- `intercom_send` can request an automated wake with `wake=true`. The receiver accepts at most eight automated wakes per session. Passive sends remain available after the limit.
- The fullscreen TUI queues eligible peer traffic during an active run and starts it when idle.
- Human steering and cancellation still use Notch's normal higher-priority controls. Peer traffic uses a follow-up, so it runs before an automatic `/goal` continuation but after queued steering.
- RPC mode can queue a wakeup only while an RPC prompt is active. When RPC is idle, the message is still safely stored and a notification event is emitted. The RPC controller must start the next prompt; during that prompt the agent can use `intercom_inbox`.
- A reply (`reply_to` is set) is stored and notified but **never wakes the other agent**. This one-way wake rule prevents automatic reply loops.

Every outbound send is durably recorded in the sender's `intercom_outbound` entries before transmission, then updated after confirmed delivery. Use `/intercom request REQUEST_ID` or `intercom_request_status` to distinguish `pending_delivery`, `delivered_pending_reply`, `replied`, and the corresponding one-way reply states. A delivered request is not proof that review or QA finished; completion gates should require and inspect the reply. Each inbound request ID accepts at most one outbound reply.

Peer text is wrapped as untrusted data before a wakeup. It is never authorization to widen scope, enable tools, change permissions, or ignore the user.

## Local security and cleanup

Each canonical workspace root gets a separate hashed namespace. When project discovery is disabled, the startup directory is used instead. Normal discovery does not expose sessions from another namespace. Registry directories use mode `0700`; registry records, lock files, and Unix sockets use mode `0600`. These permissions isolate OS users, not other processes already running as the same user. The default location is:

```text
$XDG_RUNTIME_DIR/notch-intercom/<project-hash>/
```

If `XDG_RUNTIME_DIR` is unavailable, the plugin uses the user cache directory. `NOTCH_INTERCOM_DIR` can set an absolute base directory for tests or custom setups.

Normal session shutdown removes the socket and registry record. Discovery pings records and ignores stale ones. Reopening the same session safely replaces its stale socket and record. After an abrupt process exit, private stale files for sessions that are never reopened may remain until manually removed; they are not discoverable. Persistent `.lock` files are intentional names for crash-releasing OS advisory locks.

## User walkthrough and comparison

Use a real, small task with two terminal sessions. Do not start with a broad implementation.

1. Open two Notch TUI sessions in the same repository.
2. Name them `primary` and `reviewer`.
3. In `reviewer`, run `/intercom wake on`.
4. Ask `primary` to inspect one narrow bug. Ask `reviewer` to inspect one independent risk.
5. Send one finding from `reviewer` to `primary` without copying text through the terminal.
6. Send one clarification request from `primary` to `reviewer`, then reply with `/intercom reply`.
7. Repeat the same task once with existing `run_subagent`, and once with a manual text handoff.
8. Record: time to first useful finding, duplicate work, interruption cost, delivery errors, and whether the reply changed the result.
9. Turn wakeups off and confirm `/intercom inbox` remains useful in passive mode.

Use long-lived Intercom peers when durable identity or repeated exchanges add value. Prefer `run_subagent` or coordinated helpers for one bounded review. Record cost, delay, duplicate work, and human wake interventions so the coordinator can choose the simpler path next time.

## Limits

- Unix domain sockets are required.
- There is no broadcast, shared room, automatic task assignment, or cloud broker.
- Names can collide. If they do, send to the full session ID shown by `/intercom list`.
- Confirmed outbound messages, inbound messages, and settings are durable session entries. Request status only covers messages sent by the current durable session.
