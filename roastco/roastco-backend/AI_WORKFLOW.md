# AI Workflow

The assignment asks how AI tools were used while building this. Two different things deserve documenting: **AI at build time** (how this codebase was produced) and **AI at runtime** (what the product itself delegates to a model). Conflating them hides the more interesting engineering decisions, so they're separated here.

## AI at runtime (what ships)

The product's thesis is *AI authors, a deterministic engine executes*. xAI Grok (`grok-4.1-fast`, OpenAI-compatible endpoint, strict `json_schema` structured outputs) does exactly three bounded jobs:

1. **Intent → segment spec.** Output is forced into a JSON schema, then validated against a closed whitelist of fields/operators, then compiled to parameterized SQL. The model cannot reach the database; an invented field has no compiler mapping and is rejected with a human-readable error.
2. **Message drafting.** One template per campaign with `{{tokens}}` the backend fills per shopper at dispatch. One LLM call per campaign, not per recipient — no PII shipped in bulk, deterministic personalization, reviewable before launch.
3. **Stats narration.** Read-only summary of computed numbers.

`AI_MODE=mock` is a deterministic heuristic planner emitting the same JSON shapes through the same validator. It exists for offline tests and as demo insurance — the product degrades gracefully if the model provider is down.

## AI at build time (how this was made)

**Architecture design — Claude (Anthropic).** The system was designed in an extended design session before any code: the Postgres-outbox queue vs. a broker, claim-time lease+backoff, the monotonic status-rank guard, append-only event log as source of truth with `current_status` as a rebuildable cache, last-touch attribution as a column-set (idempotent) rather than a counter, and the decision to make the channel simulator adversarial (duplicates, reordering, transient failures) so robustness is demonstrated rather than claimed. The design was then audited before implementation; that audit produced six concrete fixes (among them: record `sent` as a real log event so rebuild-from-log holds, dead-letter with a logged failure event, and temporal-causality bounds on attribution).

**Implementation — Claude.** The Go services, schema, seed generator, and React frontend were pair-written with Claude against the locked design, then executed and debugged in a live sandbox: real Postgres, both services running, seeded data.

**Verification — scripted, not vibes.** A 16-check e2e suite (`scripts/e2e.sh`) was run against the live system: ingestion idempotency (re-seed produces byte-identical counts), compiler output cross-checked against hand-written SQL, SQL-injection attempts, idempotent launch replay, duplicate-callback dedup, out-of-order event protection, receipt auth, the rebuild-from-log invariant (zero mismatches), retry backoff with a killed channel service, dead-lettering after the retry budget, recovery when the channel returns inside the window, and attribution with temporal causality (a click occurring *after* an order is refused credit — caught live when a test fixture's backdated timestamp made a correct refusal look like a bug; the fixture was fixed, not the engine).

**What AI did not decide.** Product framing (Roast & Co, the marketer-centric flow), the trust-boundary placement (validate after the model, always), accepting Postgres-as-queue at this scale, and every launch/approval gate in the UX are deliberate human decisions; the model was a fast pair of hands and a sparring partner, not the architect of record.

## Log

| date | what | tool |
|---|---|---|
| 2026-06-08 | Design session: queue semantics, event-log invariants, attribution model, channel-sim adversarial behaviours; pre-implementation audit (6 fixes) | Claude |
| 2026-06-09 | Full implementation (Go backend ×3 binaries, React frontend), live e2e: 16/16 passing | Claude |
| | _add entries as the project evolves (deploy, demo recording, fixes)_ | |
