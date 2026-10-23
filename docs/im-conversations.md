# IM conversations, surfaces, and rendering

The IM runtime uses one conversation model across Matrix, Telegram and Discord.
CLI callbacks continue to work independently of this presentation layer.

## Boundaries

- `conversation.Target` identifies a channel, room/chat, thread/topic, reply anchor,
  current sender and optional binding owner. Its `BindingKey()` uses the existing
  SessionRegistry hash format. Reply anchors and current senders do not change a
  shared conversation's identity. `conversation.Reference` identifies an outgoing
  message or reaction within that target.
- `channel.Surface` binds send, reply, update, thread creation, typing, reactions,
  read receipts, questions and links to a target. Optional connector capabilities
  choose native implementations. Platform ID conversions live in connectors.
- `event.Event` represents runtime lifecycle, tools, subagents, waiting, plans,
  notifications, confirmation requests and completion without transport markup.
  Agent callbacks expose these events alongside the legacy CLI callbacks.
- `rendering.Run` applies notification policy, renders structured state, throttles
  updates and owns final delivery. An editable surface uses one status message;
  completion replaces it with a compact terminal heading and the full result.
  Failed or unavailable edits send a replacement in the same conversation.
- `im_runtime.go` wires sessions, tools, input routing and run presenters.
  `im_sessions.go` owns binding resolution and `im_session_commands.go` implements
  session actions. `main.go` retains application/CLI setup and lifecycle commands.

## Session and thread behavior

`!session new [name]` on Matrix creates a named bot-authored root in the room and
binds its new thread to a new session. The root describes the session and how to
continue; the response links to it. Names may contain spaces or Unicode and are
stored in `metadata.json`, independently of opaque session directory IDs.
Discord uses the same semantic action through its thread capability. Telegram
keeps existing forum topics as stable conversations; creating a topic requires
platform permissions and is not assumed. A new session from the main chat uses
that chat when new-thread creation is unavailable; a request to replace an
existing topic's session is rejected explicitly.

A thread binding is immutable under normal session switch operations, including
at the SessionRegistry boundary. Switching to an already bound session gives its
existing conversation link where available. It does not move the current thread
or share one mutable Agent between two targets. Room bindings can switch while
idle. During a run, session lists and new sibling threads remain usable; requests
to mutate the current running conversation are rejected instead of blocking.

Session lists display independent names, stable selectable IDs, external user
turn counts, previews and links when supported. Lists read atomically committed
history and metadata snapshots, so a run waiting for an answer cannot lock out
session discovery. Supplemental `ask_user` user messages are marked locally as
`InputReply`; they neither increment external turn counts nor replace previews.
Persisted bindings retain their original keys, session IDs and histories. New
external conversations use collision-resistant identifiers. Discord restores
registered thread polling targets at startup and initializes cursors past old
messages.

## Inputs and notifications

Attachments keep the existing shared normalization, staging, conversion,
Vision, byte-limit, warning and GC pipeline. The incoming target supplies routing
and sender context. Pending `ask_user` answers are scoped to the target, session,
sender and request ID before the chat/run lock is acquired. Their multimodal
parts are appended after the complete tool-response batch in the same run.
Session commands remain commands while a question is waiting.

Each session has one active run. New conversation inputs enter that run's ordered
inbox instead of waiting for the chat lock to start another turn. Text and
attachments use the same normalized `UserInput` path. Attachment preparation
reserves its place before conversion, so slower downloads cannot reorder inputs
or race with final history persistence and temporary-file cleanup.

Stop and abort controls cancel the active run immediately. Scoped confirmation
responses and pending `ask_user` answers take priority over steering. The agent
consumes steering before another model call and after a complete tool-result
batch. Updates arriving before a tool starts can defer it for replanning; tools
already executing finish normally. An atomic inbox check closes admission before
run completion, and the session claim remains held through history persistence.
Messages arriving after admission closes wait for release before starting a run.

The CLI accepts steering and `@path` attachments while a run is active, using
the same inbox. Ctrl+C stops the active run; stdin EOF waits for it to finish
and persist. Commands that modify session history are rejected during a run.
If startup cannot load history, normalized requests are preserved privately in
the session's `failed-inbox.json` and the error explains that they were not
processed. This recovery file is not automatically replayed.

Questions use native interactions where available, such as Telegram keyboards
inside the original topic, and numbered text elsewhere. Confirmation questions
are mandatory interactions even under the quiet notification preset. Their
waiter is registered before the question is emitted. Background evaluator
notifications use a separately captured event sink bound to the original surface,
so restoring or closing a run's presenter cannot redirect a delayed notification.
Cron delivery likewise resolves a structured target and uses the event renderer.
Legacy flat cron delivery strings remain a compatibility fallback.

Pipeline agent nodes forward their interaction and progress events to the parent
run. Confirmation prompts remain visible, and each `ask_user` question and its
waiting status are delivered once.

The default balanced policy gives editable surfaces a single running status card
and coalesces plans into it. Quiet mode omits intermediate cards but preserves
questions and final results. Explicit category filters still take precedence.
Filtered events do not populate the state retained for later card updates.
Long final results use native connector chunking rather than truncation.

## Fallbacks

Unsupported optional affordances such as typing or reactions do not prevent a
run. Update failures fall back to sending in the same surface. An adapter that
cannot deliver into a requested thread returns an explicit unsupported error;
it does not silently post into the room. Missing deep links render plain names
and IDs. Old connector APIs and `IncomingMessage` fields remain compatibility
adapters; new runtime paths consume `Target()` and `Surface`.

Tests use fake models, channels and local HTTP protocol fixtures. They cover
capability fallback, thread routing and restoration, session binding invariants,
metadata/preview persistence, pending multimodal answers, renderer completion,
notification policies and CLI callback compatibility.
