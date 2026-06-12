-- Roast & Co — AI-native mini CRM schema
-- Decisions encoded here:
--   * external_id upsert keys make ingestion idempotent
--   * communication_events is an append-only log; UNIQUE(event_id) makes callbacks idempotent
--   * status_rank() makes status monotonic (out-of-order callbacks are harmless)
--   * UNIQUE(campaign_id, customer_id) guarantees one message per shopper per campaign
--   * (current_status, next_attempt_at) index serves the SKIP LOCKED claim

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS customers (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  external_id text NOT NULL UNIQUE,
  name        text NOT NULL,
  email       text NOT NULL DEFAULT '',
  phone       text NOT NULL DEFAULT '',
  city        text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS products (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  external_id text NOT NULL UNIQUE,
  name        text NOT NULL,
  category    text NOT NULL CHECK (category IN ('beans','ground','equipment','accessories')),
  price       numeric(12,2) NOT NULL
);

CREATE TABLE IF NOT EXISTS segments (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name          text NOT NULL,
  definition    jsonb NOT NULL,
  source_intent text NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS campaigns (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  segment_id          uuid REFERENCES segments(id),
  name                text NOT NULL,
  channel             text NOT NULL CHECK (channel IN ('email','sms','whatsapp','rcs')),
  message             text NOT NULL,
  status              text NOT NULL DEFAULT 'launched',
  definition_snapshot jsonb NOT NULL,
  source_intent       text NOT NULL DEFAULT '',
  idempotency_key     text UNIQUE,
  launched_at         timestamptz NOT NULL DEFAULT now(),
  created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS orders (
  id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  external_id            text NOT NULL UNIQUE,
  customer_id            uuid NOT NULL REFERENCES customers(id),
  ordered_at             timestamptz NOT NULL,
  total_amount           numeric(12,2) NOT NULL DEFAULT 0,
  attributed_campaign_id uuid REFERENCES campaigns(id),
  created_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_orders_customer ON orders(customer_id, ordered_at DESC);

CREATE TABLE IF NOT EXISTS order_items (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  order_id   uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
  product_id uuid NOT NULL REFERENCES products(id),
  quantity   int NOT NULL CHECK (quantity > 0),
  unit_price numeric(12,2) NOT NULL  -- price snapshot at purchase time
);
CREATE INDEX IF NOT EXISTS idx_items_order   ON order_items(order_id);
CREATE INDEX IF NOT EXISTS idx_items_product ON order_items(product_id);

CREATE TABLE IF NOT EXISTS communications (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  campaign_id     uuid NOT NULL REFERENCES campaigns(id),
  customer_id     uuid NOT NULL REFERENCES customers(id),
  channel         text NOT NULL,
  recipient       text NOT NULL,            -- resolved snapshot (email/phone at launch)
  current_status  text NOT NULL DEFAULT 'queued',
  attempt_count   int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  last_error      text NOT NULL DEFAULT '',
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (campaign_id, customer_id)         -- nobody is messaged twice in one campaign
);
CREATE INDEX IF NOT EXISTS idx_comms_claim    ON communications(current_status, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_comms_campaign ON communications(campaign_id);
CREATE INDEX IF NOT EXISTS idx_comms_customer ON communications(customer_id);

CREATE TABLE IF NOT EXISTS communication_events (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  communication_id uuid NOT NULL REFERENCES communications(id),
  event_id         text NOT NULL UNIQUE,    -- provider event id: the dedup key
  event_type       text NOT NULL,
  occurred_at      timestamptz NOT NULL,
  received_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_events_comm ON communication_events(communication_id);

-- Monotonic status ranking. 'failed' (25) outranks queued/sent/delivered but not
-- opened/read/clicked, so a failure can never erase observed engagement.
CREATE OR REPLACE FUNCTION status_rank(s text) RETURNS int
IMMUTABLE LANGUAGE sql AS $$
  SELECT CASE s
    WHEN 'queued'    THEN 0
    WHEN 'sent'      THEN 10
    WHEN 'delivered' THEN 20
    WHEN 'failed'    THEN 25
    WHEN 'opened'    THEN 30
    WHEN 'read'      THEN 40
    WHEN 'clicked'   THEN 50
    ELSE -1 END
$$;
