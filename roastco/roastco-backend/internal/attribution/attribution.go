// Package attribution implements last-touch attribution within a window —
// the industry-standard heuristic, stated honestly as a heuristic, not proof
// of causation. It runs at order-ingest time, which makes the order endpoint
// the live heartbeat: backfill and live orders share one path.
package attribution

import (
	"context"
	"database/sql"
	"os"
	"strconv"

	"github.com/roastco/backend/internal/store"
)

type Attributor struct {
	s          *store.Store
	windowDays int
}

func New(s *store.Store) *Attributor {
	days := 7
	if v := os.Getenv("ATTRIBUTION_WINDOW_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	return &Attributor{s: s, windowDays: days}
}

// Attribute (re)evaluates one order. The engagement bar is per-channel:
// 'clicked' is the qualifying touch everywhere (cleanest causal story);
// SMS — a click-poor channel — falls back to 'delivered'. The most recent
// qualifying touch inside the window wins. Setting a COLUMN (not bumping a
// counter) makes re-ingestion idempotent by construction.
func (a *Attributor) Attribute(ctx context.Context, orderID string) (attributedTo string, err error) {
	var campaignID sql.NullString
	err = a.s.DB.QueryRowContext(ctx, `
		WITH ord AS (SELECT id, customer_id, ordered_at FROM orders WHERE id = $1)
		SELECT m.campaign_id
		FROM communications m
		JOIN communication_events e ON e.communication_id = m.id
		JOIN ord ON ord.customer_id = m.customer_id
		WHERE (
		        e.event_type = 'clicked'
		     OR (m.channel = 'sms' AND e.event_type = 'delivered')
		      )
		  AND e.occurred_at <= ord.ordered_at
		  AND e.occurred_at >= ord.ordered_at - make_interval(days => $2)
		ORDER BY e.occurred_at DESC
		LIMIT 1`, orderID, a.windowDays).Scan(&campaignID)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}

	// Always write the result — including NULL — so re-evaluation is honest.
	var val interface{}
	if campaignID.Valid {
		val = campaignID.String
		attributedTo = campaignID.String
	}
	_, err = a.s.DB.ExecContext(ctx, `UPDATE orders SET attributed_campaign_id = $1 WHERE id = $2`, val, orderID)
	return attributedTo, err
}

func (a *Attributor) WindowDays() int { return a.windowDays }
