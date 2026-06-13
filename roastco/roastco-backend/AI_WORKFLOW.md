# AI Workflow — How This Project Was Built

This project is AI-native twice over: the **product** uses an LLM for three bounded jobs behind a validation boundary, and the **process** used AI the same way — as a proposer whose output I directed, challenged, and verified before anything shipped. The principle was identical in both places: *AI proposes, I decide, tests prove.*

What follows is the honest log of that process, phase by phase.

---

## Phase 0 — Domain research before any code

Before touching architecture I used AI as a research analyst to understand how this domain actually works in the real world:

- **How real channel providers behave** — studied the send-API + asynchronous-webhook pattern that Twilio, Meta (WhatsApp Business), and email providers all share: delivery receipts arrive late, out of order, and at-least-once. This directly shaped the requirement that my receipt handler be idempotent and order-insensitive, and that my channel simulator be *adversarial* (duplicates, reordering, transient failures) rather than a happy-path stub.
- **How marketing CRMs segment** — RFM (recency / frequency / monetary) is the industry's segmentation backbone. My whitelist fields (`days_since_last_order`, `order_count`, `total_spend`, category behaviour) are deliberately an RFM vocabulary, not an arbitrary field list.
- **How attribution is really done** — last-touch is the standard heuristic with known over-crediting bias; holdout groups are the gold standard. I shipped last-touch *labeled as a heuristic* and documented the upgrade path, rather than pretending the problem is simpler than it is.

## Phase 1 — Product scoping (my calls)

The brief rewards "bold, opinionated product choices rather than building everything shallowly." I made three:

1. **The bet: AI authors, a deterministic engine executes.** The LLM proposes segments, drafts messages, narrates stats — and nothing more. No autonomous sending, no model-written SQL, human approval gates before launch.
2. **Depth on the graded core.** The brief literally names the channel loop — volume, ordering, retries, failures — as the system-design test. I cut auth, multi-tenancy, scheduling, and A/B testing to go deep there instead.
3. **A concrete brand.** "Roast & Co" with a real four-category catalog forces real decisions: the AI must map "grinder" → equipment, personas make the seed data behave like a retail base, and personalization tokens mean something on screen.

## Phase 2 — Architecture as a debate, not a prompt

For each major component I had AI lay out the option space with tradeoffs, then I picked — and the rejected options are part of the record:

| Decision | Options considered | Why I chose what I chose |
|---|---|---|
| Delivery queue | Kafka / Redis / SQS **vs Postgres `FOR UPDATE SKIP LOCKED`** | Postgres gives async + concurrent workers + retries + dead-letter with zero new infra, **plus** the launch transaction creates the queue rows atomically (transactional outbox, no dual-write problem). Crossover to a broker: ~10k+ sends/min — and the dispatch package is isolated so that split is a `main()` file, not a refactor. |
| AI → database boundary | Text-to-SQL **vs structured spec + whitelist validator + compiler** | Text-to-SQL hands the model the keys: injection-shaped risk, hallucinated columns, unreviewable queries. A closed spec gives a validation surface, marketer-editable rules, and deterministic re-execution. Bounded expressiveness (8 fields) is a feature in a marketing tool. |
| State model | Mutable status column **vs append-only event log + derived status** | The log is the source of truth; `current_status` is a rebuildable cache guarded by a monotonic rank function. This one decision makes duplicates and out-of-order callbacks structurally harmless — and it's provable (see Phase 4). |
| Retry mechanics | Separate lease/janitor process **vs lease-and-backoff at claim time** | Writing `attempt_count++` and a pushed `next_attempt_at` *before* the send attempt means a crashed worker needs zero cleanup — its rows simply come due again. One SQL statement carries leasing, backoff, and crash-safety. |

## Phase 3 — Implementation discipline

Code was written with AI as the implementation pair, in small reviewed passes, under standing rules I enforced on every diff:

- **Idempotency at every boundary** — `external_id` on ingest, `Idempotency-Key` on launch, `event_id` on callbacks. If an operation could be retried, it had to be safe to retry.
- **All writes as "set", never "increment"** — attribution sets a column; status sets a max. Replays converge instead of double-counting.
- **Values always parameterized** — nothing user- or model-supplied is ever concatenated into SQL.
- **No silent failure paths** — errors either retry by design or land in a dead-letter state with the reason logged as a real event.

## Phase 4 — Adversarial review of my own system

Before calling it done, I ran a deliberate red-team pass: I asked AI to attack the design as a hostile senior reviewer, then triaged every finding myself. Concrete changes that came out of it:

- **`sent` became a first-class logged event** (not just a status write) — without it, the "rebuild every status from the log" invariant didn't actually hold.
- **`failed` got rank 25** — above `delivered`, below `opened` — encoding a real business rule: a failure report can never erase engagement we already observed.
- **Permanent vs transient send errors split** — a 4xx recipient problem fails fast; a 5xx/network problem retries with backoff. (A later production incident refined this further — see Phase 6.)
- Findings I consciously **accepted as scope** rather than fixed — float money in the Go layer, last-touch bias, no graceful shutdown — are documented as known tradeoffs, not hidden.

## Phase 5 — Verification: tests prove, diffs don't

Nothing was trusted because it "looked right." A 16-check black-box e2e suite (`scripts/e2e.sh`) runs the whole stack over HTTP and SQL and asserts, among others:

- Double launch with one idempotency key → one campaign, `replayed: true`.
- A duplicated callback → stored exactly once; an out-of-order `delivered` after `clicked` → logged but cannot regress status.
- A hostile segment value (`Gurgaon' OR 1=1 --`) → inert data, table intact; an invented field → rejected at validation.
- **The headline invariant:** recomputing every status from the append-only event log alone produces zero mismatches against the live cache.
- The seed is deterministic (RNG seed 42) — same world every run, so failures are reproducible, not flaky.

I also drilled failure modes manually: killed the channel mid-campaign and watched attempts climb the 5/10/20/40/80s backoff, then recover on restart inside the retry budget — or dead-letter cleanly with the error preserved when I let the budget expire.

## Phase 6 — Production hardening (the bugs were the best teachers)

Deploying to Railway + Vercel + Supabase surfaced real-world failures, each diagnosed from logs and turned into a structural fix rather than a one-off patch:

- **One pasted leading space in `CHANNEL_URL`** made the URL unparseable; an ignored error then dereferenced a nil request and **crash-looped the CRM** — which also explained intermittent 502s on unrelated endpoints. Diagnosis came from noticing a double space in a startup log line. Fix: trim all dashboard-pasted config at the source, never discard request-construction errors, and **reproduce the exact failure against the patch** to prove the crash class was eliminated.
- **A secret with trailing whitespace** then caused 401s between the services — the trim fix had made one side clean while the other still compared raw. Fix: consistent trimming at every secret reader.
- **Bulk-seeding a cross-region database** exposed real arithmetic: row-at-a-time ingestion (~9 round trips per order) is fine at local latency and *hours* at inter-continental latency. No timeout setting beats 24,000 round trips. Fix: recognize that one-time bulk loading is a different job with a different right tool — a dependency-ordered, multi-row-INSERT data dump replayed in ~20 statements.
- **Supabase free-tier pausing** wiped the production schema once; a scheduled GitHub Action now pings the health endpoint (which pings the DB) twice weekly so the demo cannot pause out from under an evaluator.

## Where AI lives in the shipped product

Exactly three jobs, all behind the validator and human approval: intent → segment spec, message-template + channel drafting, stats narration. One model call per campaign (not per recipient — cost, privacy, determinism). Provider auto-detected from the key (Groq / xAI), with a json_schema → json_object downgrade path and a deterministic mock planner for offline tests and demo resilience — safe to swap because the contract is the validated spec, not the model.

## Honest limits

AI accelerated every phase of this project — research, design exploration, implementation, review. What it never did was decide. The scoping, the architecture choices, the accepted tradeoffs, the verification bar, and the production debugging judgment are mine, and I can defend any line of this repository on those terms.
