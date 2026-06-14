# Roast & Co — AI-Native Mini CRM

> A marketer types an audience in plain English. The AI proposes the targeting rules and the message. A deterministic engine sends through a deliberately unreliable channel, statuses stream back live, and orders that follow a click get attributed to the campaign — revenue, not just clicks.

Built for the **Xeno Engineering Internship Assignment 2026**.

**Live demo:** https://roast-co-crm.vercel.app

**Walkthrough video:**(https://drive.google.com/file/d/18l6MlCbeF8zskpajv06Efcczu8A6ga7_/view?usp=sharing)

---

## The core bet

**AI authors, a deterministic engine executes.**

The LLM does exactly three bounded jobs — (1) intent → segment spec, (2) message-template drafting with a channel suggestion, (3) stats narration — all behind human approval gates. It never writes SQL, never sends a message, never touches a shopper record. Everything it proposes passes through a whitelist validator before it can have any effect, which makes hallucination-safety and injection-safety the *same mechanism*: an invented field has no compiler mapping and cannot execute.

## What it does (60-second tour)

1. **New campaign** → type an intent like *"win back customers who bought beans in the last 6 months but haven't ordered in 60 days — 20% off"*.
2. The AI returns **editable structured rules** + a live audience count. Edit a rule → the count re-runs deterministically, no AI involved.
3. **Draft with AI** → one message template with `{{personalization}}` tokens and a suggested channel (with a reason). Launch is idempotent — double-clicks replay, they don't re-send.
4. On the campaign page, open **Message Threads** → pick a shopper → see the exact rendered message they received, with WhatsApp-style ticks advancing live (✓ sent → ✓✓ delivered → ✓✓ read → *clicked*) as the channel's callbacks arrive.
5. **Simulate incoming order** → the order attributes to the campaign (last qualifying touch within a 7-day window) and attributed revenue appears.

## Architecture

```
┌──────────────┐   JSON API    ┌─────────────────────────────┐  POST /send   ┌──────────────────┐
│   React SPA  │ ────────────▶ │        CRM (Go)             │ ────────────▶ │  Channel (Go)    │
│   (Vercel)   │ ◀──────────── │  HTTP API · AI planner ·    │ ◀──────────── │  stubbed provider│
└──────────────┘    polling    │  dispatch worker pool       │   callbacks   │  adversarial sim │
                               └──────────┬──────────────────┘  /api/receipts└──────────────────┘
                                          │ SQL (pooled)
                              ┌───────────▼───────────┐        ┌─────────────┐
                              │  Postgres (Supabase)  │        │  Groq LLM   │
                              │  data + outbox queue  │        │ 3 bounded   │
                              │  + append-only events │        │ jobs only   │
                              └───────────────────────┘        └─────────────┘
```

Two independently deployed Go services. The channel knows nothing about the CRM except the callback URL inside each send request — exactly how real providers (Twilio, Meta) work.

### The delivery loop — volume, ordering, retries, failures

- **Queue:** Postgres itself, via `SELECT … FOR UPDATE SKIP LOCKED` — concurrent workers claim disjoint batches with no coordinator. Launch inserts campaign + audience in **one transaction** (the transactional-outbox property: a campaign can never half-exist).
- **Retries:** `attempt_count++` and a pushed `next_attempt_at` are written *at claim time* — lease and exponential backoff (5/10/20/40/80s) in one statement, so a crashed worker's rows simply come due again. Five attempts → dead-letter with the error logged as a real event.
- **The channel is adversarial on purpose:** random latency (so callbacks arrive out of order), transient 503s, ~6% lifecycle failures, and ~12% of callbacks deliberately **duplicated** — every campaign exercises the hard paths in normal operation.
- **Idempotent, monotonic receipts:** `INSERT … ON CONFLICT (event_id) DO NOTHING` absorbs duplicates; a rank-guarded `UPDATE` (`status_rank(new) > status_rank(current)`) means status only climbs, so out-of-order delivery is harmless. At-least-once delivery + idempotent receiver = exactly-once effect.
- **The log is the truth:** `communication_events` is append-only; `current_status` is a rebuildable cache — the e2e suite reconstructs every status from the log alone and asserts **zero mismatches**.
- **Attribution:** last qualifying touch (clicked; delivered-fallback for SMS) *before* the order, inside the window. It sets a column, never increments a counter — re-ingesting an order is idempotent by construction, and a click that happens *after* a purchase gets no credit.

## Repository layout

```
.
├── .github/workflows/keepalive.yml     # pings prod twice weekly so the free-tier DB never pauses
└── roastco/
    ├── roastco-backend/                # one Go module, three binaries — see its README for API docs
    │   ├── cmd/crm                     #   CRM: HTTP API + dispatch workers, migrates on boot
    │   ├── cmd/channel                 #   stubbed channel provider (separate deployment)
    │   ├── cmd/seed                    #   deterministic world generator (seed 42 → identical every run)
    │   ├── internal/…                  #   ai · api · attribution · channelsim · dispatch · segment · store · wire
    │   ├── migrations/                 #   schema, embedded into the binary
    │   └── scripts/e2e.sh              #   16-check black-box test suite
    └── roastco-frontend/               # React + Vite SPA — see its README
```

## Tech stack

| Layer | Choice | Why |
|---|---|---|
| Backend | Go 1.22, `net/http`, `lib/pq` | Small, explicit, zero frameworks; concurrency primitives fit a dispatch pool naturally |
| Database | PostgreSQL (Supabase) | Storage **and** queue **and** dedup **and** ordering guard — four jobs, one system, transactional launch |
| AI | Groq / xAI via structured outputs, auto-detected by key; deterministic mock for offline | The contract is the validated spec, not the model — providers are swappable |
| Frontend | React 18 + Vite, hand-written CSS | One stylesheet, no UI framework; polling keeps it debuggable at demo scale |
| Hosting | Railway (2 services) + Vercel + Supabase | Free-tier friendly; deploy order: channel → CRM → frontend |

## Run locally

Prereqs: Go 1.22+, Node 18+, PostgreSQL 14+.

```bash
# 1. database
createdb roastco

# 2. config — copy and fill (defaults work for local; AI_MODE=mock needs no key)
cp roastco/roastco-backend/.env.example roastco/roastco-backend/.env

# 3. three terminals in roastco/roastco-backend
go run ./cmd/channel     # → channel service listening on :8081
go run ./cmd/crm         # → migrations applied … crm listening on :8082
go run ./cmd/seed        # → 42 products, 800 shoppers, ~2.6k orders in seconds

# 4. frontend, in roastco/roastco-frontend
npm install && npm run dev    # → http://localhost:5173
```

Run the test suite (requires the stack up): `bash roastco/roastco-backend/scripts/e2e.sh` — 16 black-box checks covering idempotent launches, duplicate callbacks, out-of-order events, injection attempts, dead-lettering, log-rebuild, and attribution.

## Configuration

All knobs are environment variables (full table in the backend README). The ones that matter most:

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | local | Postgres DSN (Supabase: use the **session** pooler — IPv4-reachable, prepared-statement safe) |
| `CHANNEL_SHARED_SECRET` | dev value | Authenticates both directions; must match on both services |
| `AI_MODE` / `XAI_API_KEY` | `grok` | `grok` = live LLM (gsk_→Groq, xai-→xAI auto-detected); `mock` = offline deterministic planner |
| `WORKER_COUNT` / `DISPATCH_BATCH` | 6 / 20 | Dispatch throughput = workers × batch ÷ latency |
| `DISPATCH_MAX_ATTEMPTS` / `DISPATCH_BASE_BACKOFF_SEC` | 5 / 5 | Retry budget and backoff base (5→10→20→40→80s) |
| `CHANNEL_MIN/MAX_DELAY_MS` | 800/6000 | Channel pacing — raise for slow-motion demos |
| `ATTRIBUTION_WINDOW_DAYS` | 7 | Last-touch window |

In production, `PORT` is injected by the platform — never set it manually.

## Conscious tradeoffs (and what changes at scale)

Stated up front, as the assignment asks:

- **Postgres as the queue** — gives async + concurrent consumers + retries + DLQ with zero extra infra *and* a transactional launch. A broker (Kafka/SQS) earns its place around ~10k+ sends/min; the dispatch package is already isolated for that split.
- **Live aggregate stats** — correct and instant at thousands of communications; at millions, move to incremental rollup counters maintained by the receipt handler.
- **Last-touch attribution** — the industry-standard heuristic, labeled as one; holdout groups are the at-scale answer for true causal lift.
- **Polling UI over WebSockets** — simpler and debuggable at demo volume.
- **No auth / single tenant** — out of scope for a single-marketer demo; the schema doesn't preclude a `tenant_id`.

## AI-native development

How this was built is documented honestly in [`AI_WORKFLOW.md`](roastco/roastco-backend/AI_WORKFLOW.md) — AI as a design sparring partner first (the architecture audit produced real fixes, like logging `sent` as a first-class event so the rebuild-from-log invariant holds), then as an implementation pair, with every decision owned and everything verified by the scripted e2e suite rather than by reading diffs and hoping.

---

*Roast & Co is a fictional specialty-coffee brand; all shoppers, orders, and phone numbers are generated data (deterministic seed 42).*
