# Terminal Task transitions resolve queued Rider messages

A queued Lead instruction could remain `queued` after its Rider reported done and moved to `reported`. The Task could no longer receive it, the Lead got no warning, and retries skipped the Task forever.

## Decision

- Treat entry into `reported`, `failed`, `lost`, `landed` or `torn-down` as a boundary after which queued and claimed messages cannot be delivered.
- In the same database transaction as the Task state change, change those messages to terminal `undeliverable` and create one `queued_message_undeliverable` Notice listing their message IDs and a supported resend step. The migration applies the same resolution to existing terminal Tasks with orphaned messages.
- Keep the message bodies and IDs inspectable through `posse show <task>` until Task history is removed.
- Make queue insertion and submission conditional on the Task still accepting instructions, so a concurrent terminal transition cannot create or submit a late queued message.
- Leave `submitting` messages to the existing uncertain-delivery protocol; Herdr may already have accepted them.
- Preserve queued delivery FIFO ordering and focus-safe behavior for Tasks that can still receive instructions.

## Consequences

A failed or lost Task's Notice recommends relaunching before resending. Reported, landed and torn-down Tasks require a new Ship Task for resending. A message is never silently retried against a later Task or a replacement Rider session.
