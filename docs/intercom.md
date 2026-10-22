# Built-in local Intercom

Intercom lets running Notch sessions in the same project send durable requests
and replies. It is built into the binary; no plugin, daemon, or extra runtime is
needed. **Each durable session must opt in.** Wakeups need a separate opt-in.
Nothing listens or polls while Intercom is disabled.

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

The model has six tools: `intercom_list`, `intercom_send`, `intercom_reply`,
`intercom_inbox`, `intercom_request_status`, and `intercom_retry`. Sends from the
model are passive unless it explicitly sets `wake: true`. The slash-command
send requests a wakeup. Neither can bypass the recipient's wake setting.

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

That recovery command requires enabled wakeups and uses the same wake budget.
It can repeat model work, so inspect any existing reply first. There is no claim
of exactly-once model execution. External actions taken by a model are not
transactional with message delivery.

## Safety and limits

- Unix domain sockets and file locks are required (Linux and macOS).
- Local peers run as the same OS user. The private socket directory and files
  keep other OS users out; this is not a sandbox against code running as you.
- Peer text is untrusted data, not permission to change scope or use tools.
- Requests and correlated replies share a durable limit of eight wake attempts
  per session, including failed wake attempts and explicit recovery. A second
  reply to the same request does not create another automatic wake. New request
  chains also consume this budget, which bounds reply loops and model costs.
- Wakeups off means passive inbox delivery. Hitting the wake budget also keeps
  messages in the inbox; read them and ask the agent directly if more work is
  needed. There is no timer that resets the budget.
- Message bodies are limited to 16 KiB. A session accepts at most 1,000 incoming
  and 1,000 outgoing messages. Older messages are not silently discarded to make
  room for duplicates. Use a new session when a history limit is reached.
- Discovery checks live peers on demand. Each enabled session owns one listener;
  shutdown or a session switch closes it. Idle listeners do not poll. Connections
  have deadlines and concurrency limits.
- Intercom wakes an idle **running** process. It does not launch a closed Notch.

Runtime discovery files use `$XDG_RUNTIME_DIR/notch-intercom` when available,
otherwise the OS user cache directory under `notch/intercom`. The runtime base
and project directories must be private and owned by the current user, with no
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
