# ADR-0019: Message templates

- Status: Accepted
- Date: 2026-11-06

## Context

Message templates with variables are an optional bonus in the brief, approved by the maintainer
as Phase 6b. A client stores text such as `Hi {{name}}, your code is {{code}}` once and then sends
notifications by naming the template and giving the values. Three questions were open: when the
values are filled in, what the placeholder syntax is, and whether a template can change.

## Options and decision

### 1. When variables are filled in

- A. When the request arrives. The notification stores the final text.
- B. When the worker sends it.

**Decision: A** (chosen by the maintainer). The stored `content` is exactly what is sent, so the
worker, the retry logic and the provider client do not change, and a retry can never produce
different text from the first attempt. A template problem (an unknown template, a missing value,
a rendered text over the SMS limit) is a `422` the client sees at once. With B the same problem
would surface minutes later as a dead letter, and editing a template would change messages
already in the queue.

### 2. Placeholder syntax

- A. Strict `{{name}}` placeholders: no logic, a missing value or an unused value is an error.
- B. Go's `text/template`, with conditions and loops.
- C. A, plus default values such as `{{name|there}}`.

**Decision: A** (chosen by the maintainer). It covers what notification text needs and is easy to
explain and test. B's error messages are written for Go developers, not API clients, and its
power (function calls, loops) is more than message text should have. C adds parsing rules for a
convenience the client can provide itself.

Being strict about extra values catches typos: with `{"nme": "Ann"}` the template's `{{name}}`
would otherwise silently stay empty. Being strict about malformed placeholders at creation time
(`{{first name}}`, an unclosed `{{name`) stops broken text from reaching customers.

### 3. Whether a template can change

- A. Immutable: create, get and list. A new wording is a new template.
- B. Editable with `PUT` and a version number kept with each notification.
- C. Editable in place, without versions.

**Decision: A** (chosen by the maintainer). A template ID always means the same text, so a stored
notification can always be traced to the text it came from, and an idempotent replay cannot meet
a template that changed in between. B adds a versions table and a version per notification; C
loses the history. B stays possible later without breaking the API.

## How it works

- `POST /v1/templates` validates the name (unique, a slug), the channel and the body. The text
  outside the placeholders must already fit the channel's limit; otherwise no notification could
  ever be rendered from it. At most 50 different variables.
- Creating the same name, channel and body again returns the stored template with `200` and
  `Idempotent-Replayed: true`. The same name with different content is `409
  template_name_taken`. The unique name gives safe retries without an `Idempotency-Key`.
- A notification or batch item gives `templateId` and `variables` instead of `content`. The
  channel may be omitted and defaults to the template's; a different channel is a `422`.
- The handler loads every template a request names in one query (`WHERE id = ANY(...)`), so a
  batch of 1000 items using three templates costs one round trip. It renders each item, then runs
  the normal validation on the rendered text, so the SMS limit applies to what is actually sent.
- Values are inserted literally and are not rendered again: a value containing `{{x}}` stays text.
- `template_id` is stored on the notification (migration `000004`, a foreign key) and returned as
  `templateId`. The idempotency hash covers the template ID and the rendered text, so the same key
  with different values is a `409`; unchanged requests without a template hash exactly as before.

## Consequences

- No change to the worker, the queue or the provider contract.
- The API now reads the database before it reports validation errors for templated requests. Plain
  requests are unaffected, and A-41 (validation before any idempotency check) still holds.
- Templates cannot be deleted or changed. Unused templates stay in the table; with tenants and real
  volume, an archive flag or version history (option B) would be the next step.
- Errors about the rendered text are reported on the field `content`, which the client did not send;
  the spec says so (A-44).
