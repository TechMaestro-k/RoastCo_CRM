# Roast & Co — AI-native Mini CRM (backend)

The campaign engine behind **Roast & Co**, a fictional specialty-coffee brand. A marketer describes an audience in plain English; the AI proposes structured targeting rules and a message template; a deterministic engine executes the send through a deliberately unreliable channel and proves, with attributed orders, whether the campaign made money.

**The bet:** AI authors, a deterministic engine executes. The LLM does exactly three bounded jobs behind human approval gates — intent → segment spec, message-template drafting (with channel suggestion), and stats narration. It never writes SQL, never sends a message, never touches a shopper record.

Frontend lives in its own repo (React + Vite). This repo holds one Go module with three binaries:

| binary | what it is |
|---|---|
| `cmd/crm` | the CRM: HTTP API + dispatch worker pool, migrates on boot |
| `cmd/channel` | the stubbed channel provider — adversarial on purpose (latency, 5xx, duplicate callbacks, out-of-order lifecycles) |
| `cmd/seed` | deterministic world generator (42 products, 2,500 shoppers with personas, ~13k orders) fed through the real ingest API |

## How the loop works

1. **Intent → spec.** `POST /api/segments/preview` sends the marketer's sentence to Grok with strict structured outputs. The reply is validated against a closed whitelist of fields/operators and compiled to parameterized SQL. An invented field has no mapping and cannot compile — hallucination-safety and injection-safety are the same mechanism.
2. **Edit loop.** The marketer can edit rules; `preview-spec` re-runs them with no AI involved.
3. **Launch.** One transaction inserts segment + campaign and materialises one `communications` row per matching shopper (`UNIQUE(campaign_id, customer_id)` makes double-sends structurally impossible; the `Idempotency-Key` header makes double-clicks replay, not relaunch).
4. **Dispatch.** A bounded worker pool claims due rows with `SELECT … FOR UPDATE SKIP LOCKED`. `attempt_count++` and a pushed `next_attempt_at` happen *at claim time* — lease and exponential backoff in one move, so a crashed worker's rows simply come due again. Five attempts, then dead-letter with a logged `failed` event.
5. **Receipts.** The channel calls back at-least-once, with deliberate duplicates and random ordering. `INSERT … ON CONFLICT (event_id) DO NOTHING` absorbs duplicates; a rank-guarded `UPDATE` (`status_rank(new) > status_rank(current)`) makes status monotonic, so out-of-order callbacks are harmless. The event log is the source of truth; `current_status` is a rebuildable cache — and the e2e suite proves it rebuilds exactly.
6. **Attribution.** Every ingested order is (re)evaluated: last qualifying touch (clicked; delivered fallback for SMS) within `ATTRIBUTION_WINDOW_DAYS` wins. It sets a *column*, never bumps a counter — re-ingestion is idempotent by construction.

## Run locally

Prereqs: Go 1.22+, Postgres 14+.

```bash
createdb roastco
export DATABASE_URL="postgres://localhost:5432/roastco?sslmode=disable"
export CHANNEL_SHARED_SECRET=$(openssl rand -hex 24)
export AI_MODE=mock                      # or grok + XAI_API_KEY

go run ./cmd/channel &                   # :8081
go run ./cmd/crm &                       # :8080, migrates on boot
go run ./cmd/seed                        # feeds the ingest API
```

Open the frontend (`npm run dev` in the frontend repo) or talk to the API directly — `scripts/e2e.sh` is a tour of every endpoint.

`AI_MODE=mock` is a deterministic heuristic planner producing the same JSON shapes as Grok. It exists so tests run offline and so a live demo survives a Grok outage.

## Tests

```bash
CHANNEL_SHARED_SECRET=... PSQL_CONN=postgres://localhost:5432/roastco ./scripts/e2e.sh
```

16 checks: AI preview, edit loop, whitelist + SQL-injection guards, idempotent launch, funnel settle, log completeness (every dispatched comm has a `sent` event), duplicate-callback dedup, out-of-order protection, receipt auth, the rebuild-from-log invariant, attribution, and order re-ingest idempotency. Retry/dead-letter behaviour is easiest to watch by hand: `pkill channel`, launch a small campaign, watch `attempt_count` climb on the queued rows, restart the channel inside the window (rows recover) or after ~62s (rows dead-letter with `last_error`).

## Deploy (Railway × 2 + Supabase + Vercel)

**Database — Supabase.** Create a project, copy the **direct** connection string (port 5432, not the 6543 pooler). Free tier pauses after ~7 idle days: add this repo's `keep-alive` GitHub Action with secret `CRM_URL` set to the deployed CRM URL — `/healthz` pings the database.

**Channel service — Railway.** New service from this repo. Build: `go build -o app ./cmd/channel`, start: `./app`. Env: `CHANNEL_SHARED_SECRET`. Note its public URL.

**CRM — Railway.** Second service, same repo. Build: `go build -o app ./cmd/crm`, start: `./app`. Env: `DATABASE_URL`, `CHANNEL_SHARED_SECRET` (same value), `XAI_API_KEY`, `AI_MODE=grok`, `CHANNEL_URL=https://<channel>.up.railway.app`, `CALLBACK_URL=https://<crm>.up.railway.app/api/receipts`, `FRONTEND_ORIGIN=https://<frontend>.vercel.app`.

**Seed once** from your machine: `SEED_API_URL=https://<crm>.up.railway.app go run ./cmd/seed`.

**Frontend — Vercel** (other repo): set `VITE_API_URL=https://<crm>.up.railway.app`.

## Scope honesty

No auth/multi-tenancy (single-marketer demo), no real messaging providers (the brief forbids them — the channel sim's chaos is the point), in-memory channel state (a real provider persists; stated tradeoff), derived metrics computed live per request (a rollup table is the documented next step at scale), Postgres-as-queue (`SKIP LOCKED`) instead of a broker — the table gives async + concurrent workers + retries + DLQ without new infrastructure; a broker earns its place when polling contention does.
