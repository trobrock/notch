# Built-in local Intercom

Intercom lets running Notch sessions in the same project or an explicitly named
local collaboration send durable requests and replies. It is built into the
binary; no plugin, daemon, or extra runtime is needed. **Each durable session
must opt in.** Wakeups need a separate opt-in. Nothing listens or polls while
Intercom is disabled.

## Start two sessions

Use fullscreen Notch in two terminals in the same repository. Linked Git
worktrees share a namespace; separate clones and unrelated projects do not.
Outside Git, sessions must use the same canonical workspace directory.

In the first session:

```text
/intercom name primary
/intercom on
/intercom wake on
```

In the second:

```text
/intercom name reviewer
/intercom on
/intercom wake on
```

Then, in `primary`:

```text
/intercom list
/intercom send reviewer Review the error handling and reply with your findings.
```

The recipient gets a notice. If wakeups are enabled, an idle session starts a
model run; a busy session queues the message after its current work. The agent
can use `intercom_reply` to return a result. A correlated reply can also wake the
requester, so no copy/paste or second user prompt is needed.

The model has seven tools. `intercom_configure` can set this session's name,
project or collaboration scope, messaging opt-in, and wake opt-in. The other
six are `intercom_list`, `intercom_send`, `intercom_reply`, `intercom_inbox`,
`intercom_request_status`, and `intercom_retry`. Sends from the model are
passive unless it explicitly sets `wake: true`. The slash-command send requests
a wakeup. Neither can bypass the recipient's wake setting. Enabling wakeups can
start later model turns and spend tokens, so the model should do it only when
the user explicitly asks.

To stop wakeups or messaging:

```text
/intercom wake off
/intercom off
```

Names, opt-ins, and message history are saved in the durable session. Resume
that same session to restore them. `/new` does not inherit the opt-in. Fullscreen
TUI and RPC hosts are supported; line/print/JSON runs and `--no-session` cannot
enable Intercom. Remove the old optional Intercom plugin if it is still installed;
its command and tool names would collide with the built-in extension.

## Cross-project collaborations

A durable session normally discovers only peers in its repository. Linked Git
worktrees share that repository scope. To collaborate across unrelated local
repositories, join the same named collaboration in each session:

```text
# Run while Intercom is off, in the Notch repository:
/intercom collaboration create platform-work
/intercom name notch-lead
/intercom wake on
/intercom on

# In another repository:
/intercom collaboration join platform-work
/intercom name developerly-reviewer
/intercom wake on
/intercom on
```

`create` and `join` both select the named local collaboration. There is no
global room list, daemon, or broker. Names are resolved only under the private
runtime directory of the current OS user. Changing or leaving a collaboration
turns messaging and wakeups off; enable them again explicitly. Return to the
repository-only scope with:

```text
/intercom off
/intercom collaboration leave
```

In a collaboration, peer lists and incoming messages include a workspace label,
such as `notch/notch-lead`. Use that qualified name when a plain session name is
ambiguous. Exact session IDs are always accepted.

The agent can perform the same setup with `intercom_configure`. For example, ask
it to join `platform-work`, name itself `notch-lead`, and enable messaging. It
can configure only its own durable session. Joining a named collaboration
trusts its members to request work, including edits, without repeating approval
in each target session. Requests still follow the user's instructions and
higher-priority rules. Joining does not share credentials, change provider
routing, trust repository configuration, or grant OS-level filesystem access.
Project-only messages remain untrusted.

## Delivery, replies, and recovery

Every outbound message is recorded before delivery. The receiver records the
message before acknowledging it or requesting a wakeup. Messages carry a
version, stable request ID, sender/recipient session IDs, and an optional reply ID.
Receivers deduplicate by sender and request ID and reject changed payloads using
the same ID. An outbound reply is limited to one per inbound request. A reply
cannot itself be replied to through `intercom_reply`.

```text
/intercom inbox
/intercom request REQUEST_ID
/intercom reply REQUEST_ID Your findings here
/intercom retry REQUEST_ID
```

The status distinguishes `pending_delivery`, `delivered_pending_reply`, and
`replied`, plus delivery states for outbound replies. `replied` means a matching
message arrived from the addressed session, not that its findings are correct.

A send makes at most two immediate transport attempts, using the same ID. If a
connection fails or an acknowledgment is lost, **retry the recorded ID instead of
sending a new message**. Retry also works after resuming the sender and recipient.
There is no automatic background retry or offline broker. The peer must be
running with Intercom enabled when delivery is attempted.

**Delivered means stored, not processed.** `wake_queued`, when present in a
receipt or recorded request, means the host accepted a wakeup, not that the model
finished work. Delivery warnings are retained with the sender's request status.
A crash between persistence and wakeup can leave a stored message unprocessed;
a duplicate delivery does not wake it again. Inspect the inbox and, if needed,
explicitly queue a stored message:

```text
/intercom process REQUEST_ID
```

That recovery command requires enabled wakeups. There is no lifetime wake limit; each explicit recovery is recorded for audit.
It can repeat model work, so inspect any existing reply first. There is no claim
of exactly-once model execution. External actions taken by a model are not
transactional with message delivery.

## Safety and limits

- Unix domain sockets and file locks are required (Linux and macOS).
- Local peers run as the same OS user. The private socket directory and files
  keep other OS users out; this is not a sandbox against code running as you.
- Named collaboration membership is an explicit trust decision: members may request work, including edits. Project-only peer messages remain untrusted. Neither mode overrides the user's instructions or higher-priority rules; quoted text and external content are still data.
- The receiver records collaboration membership with each new inbound message, so resume and explicit recovery preserve its original trust. Older messages without this metadata remain untrusted; ask the sender to send a new request.
- Collaboration membership can move message text into another repository's
  durable session log. Do not join a collaboration when its peers should not
  receive the current session's data.
- There is no lifetime count limit on requested wakeups, including after resume. Duplicate delivery never queues another wake, and a second reply to the same request does not create another automatic wake. Request/reply loops can consume model tokens; disable wakeups or messaging to stop them.
- Wakeups off means passive inbox delivery. Enabling wakeups later does not replay stored messages automatically; use explicit recovery when needed.
- Message bodies are limited to 16 KiB. A session accepts at most 1,000 incoming
  and 1,000 outgoing messages. Older messages are not silently discarded to make
  room for duplicates. Use a new session when a history limit is reached.
- Discovery checks live peers on demand. Each enabled session owns one listener;
  shutdown or a session switch closes it. Idle listeners do not poll. Connections
  have deadlines and concurrency limits.
- Intercom wakes an idle **running** process. It does not launch a closed Notch.

Runtime discovery files use `$XDG_RUNTIME_DIR/notch-intercom` when available,
otherwise the OS user cache directory under `notch/intercom`. The runtime base
and scope directories must be private and owned by the current user, with no
symlink substitution. Ancestors must be root/user-owned and not writable by
other users unless protected by a sticky bit (as `/tmp` is). Unsafe directories
are rejected, not silently chmodded. Ancestor symlink targets containing `..`
are refused; use a canonical runtime path instead. If a long home or
cache path exceeds the Unix socket path limit, set an absolute, private short
`NOTCH_INTERCOM_DIR` (for example `/tmp/notch-intercom-YOUR_UID`) in both terminals.
Messages themselves live in the normal append-only session JSONL, not these
runtime discovery files.

## RPC hosts

RPC clients can opt in using the generic user extension-command request:

```json
{"id":"ic-on","type":"extension_command","name":"intercom","args":"on"}
{"id":"ic-name","type":"extension_command","name":"intercom","args":"name reviewer"}
{"id":"ic-wake","type":"extension_command","name":"intercom","args":"wake on"}
```

Wait for each response before sending the next setup command. Commands can run
asynchronously, and responses are correlated by `id`. Keep stdin open and read
stdout events while waiting for peer messages. An idle Intercom wake emits the
normal agent events without a fake response to a user `prompt` request.

## Future cross-machine transport

This version exposes no network listener. Message envelopes and delivery
receipts are separate from Unix socket discovery and framing (`protocol.go` vs
`local.go`). Durable IDs, reply correlation, deduplication, and outbox retry do
not depend on socket paths.

A later SSH transport can exchange the same versioned envelopes and receipts.
It will need explicit remote project mapping, machine-aware addressing, and
sender authentication. Local filesystem project hashes and socket ownership
must not be treated as remote authentication. No distributed broker or shared
filesystem is assumed by the message protocol.
