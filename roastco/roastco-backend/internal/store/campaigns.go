package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/roastco/backend/internal/segment"
)

// ---------- segment preview ----------

type SampleCustomer struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Email      string  `json:"email"`
	City       string  `json:"city"`
	TotalSpend float64 `json:"total_spend"`
	OrderCount int     `json:"order_count"`
	DaysSince  *int    `json:"days_since_last_order"`
}

// PreviewSegment runs a compiled spec: count + a small sample. The spec is
// executed deterministically — same spec + same data = same audience.
func (s *Store) PreviewSegment(ctx context.Context, spec segment.Spec) (int, []SampleCustomer, error) {
	where, args, err := segment.Compile(spec, 1)
	if err != nil {
		return 0, nil, err
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM customers c WHERE `+where, args...).Scan(&count); err != nil {
		return 0, nil, fmt.Errorf("segment query failed: %w", err)
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT c.id, c.name, c.email, c.city,
		       (SELECT COALESCE(SUM(o.total_amount),0)::float8 FROM orders o WHERE o.customer_id=c.id),
		       (SELECT COUNT(*) FROM orders o WHERE o.customer_id=c.id),
		       (SELECT EXTRACT(day FROM now()-MAX(o.ordered_at))::int FROM orders o WHERE o.customer_id=c.id)
		FROM customers c WHERE `+where+` ORDER BY c.created_at LIMIT 8`, args...)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var sample []SampleCustomer
	for rows.Next() {
		var sc SampleCustomer
		var days sql.NullInt64
		if err := rows.Scan(&sc.ID, &sc.Name, &sc.Email, &sc.City, &sc.TotalSpend, &sc.OrderCount, &days); err != nil {
			return 0, nil, err
		}
		if days.Valid {
			d := int(days.Int64)
			sc.DaysSince = &d
		}
		sample = append(sample, sc)
	}
	return count, sample, rows.Err()
}

// ---------- campaigns ----------

type LaunchInput struct {
	Name           string       `json:"name"`
	Channel        string       `json:"channel"`
	Message        string       `json:"message"`
	Definition     segment.Spec `json:"definition"`
	SourceIntent   string       `json:"source_intent"`
	SegmentName    string       `json:"segment_name"`
	IdempotencyKey string       `json:"idempotency_key"`
}

// LaunchCampaign creates segment + campaign and materialises the audience
// into communications, all in ONE transaction — launch and queue-creation are
// atomic (the transactional-outbox property). Launch is idempotent on the
// client key: a double-click returns the existing campaign untouched.
func (s *Store) LaunchCampaign(ctx context.Context, in LaunchInput) (campaignID string, recipients int, replay bool, err error) {
	if in.IdempotencyKey != "" {
		var existing string
		err = s.DB.QueryRowContext(ctx, `SELECT id FROM campaigns WHERE idempotency_key=$1`, in.IdempotencyKey).Scan(&existing)
		if err == nil {
			var n int
			_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM communications WHERE campaign_id=$1`, existing).Scan(&n)
			return existing, n, true, nil
		}
		if err != sql.ErrNoRows {
			return "", 0, false, err
		}
	}

	// $1=campaign_id, $2=channel; the compiled WHERE starts at $3.
	where, args, err := segment.Compile(in.Definition, 3)
	if err != nil {
		return "", 0, false, err
	}
	defJSON, _ := json.Marshal(in.Definition)

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, false, err
	}
	defer tx.Rollback()

	var segmentID string
	segName := in.SegmentName
	if segName == "" {
		segName = in.Name
	}
	if err = tx.QueryRowContext(ctx, `
		INSERT INTO segments (name, definition, source_intent) VALUES ($1,$2,$3) RETURNING id`,
		segName, defJSON, in.SourceIntent).Scan(&segmentID); err != nil {
		return "", 0, false, err
	}

	var key interface{}
	if in.IdempotencyKey != "" {
		key = in.IdempotencyKey
	}
	if err = tx.QueryRowContext(ctx, `
		INSERT INTO campaigns (segment_id, name, channel, message, definition_snapshot, source_intent, idempotency_key, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'launched') RETURNING id`,
		segmentID, in.Name, in.Channel, in.Message, defJSON, in.SourceIntent, key).Scan(&campaignID); err != nil {
		return "", 0, false, err
	}

	// Materialise: one communication per matching shopper, recipient resolved
	// NOW (snapshot), channel-appropriate contact field, queued for dispatch.
	// UNIQUE(campaign_id, customer_id) is the structural double-send guard.
	// recipientExpr is a code-chosen column expression, never user data.
	recipientExpr := "c.phone"
	if in.Channel == "email" {
		recipientExpr = "c.email"
	}
	allArgs := append([]interface{}{campaignID, in.Channel}, args...)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO communications (campaign_id, customer_id, channel, recipient, current_status, next_attempt_at)
		SELECT $1::uuid, c.id, $2, `+recipientExpr+`, 'queued', now()
		FROM customers c WHERE `+where+`
		ON CONFLICT (campaign_id, customer_id) DO NOTHING`, allArgs...)
	if err != nil {
		return "", 0, false, err
	}
	n, _ := res.RowsAffected()
	return campaignID, int(n), false, tx.Commit()
}

type CampaignRow struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Channel      string    `json:"channel"`
	Message      string    `json:"message"`
	Status       string    `json:"status"`
	SourceIntent string    `json:"source_intent"`
	Definition   json.RawMessage `json:"definition"`
	LaunchedAt   time.Time `json:"launched_at"`
	Recipients   int       `json:"recipients"`
}

func (s *Store) ListCampaigns(ctx context.Context) ([]CampaignRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT g.id, g.name, g.channel, g.message, g.status, g.source_intent, g.definition_snapshot, g.launched_at,
		       (SELECT COUNT(*) FROM communications m WHERE m.campaign_id=g.id)
		FROM campaigns g ORDER BY g.launched_at DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CampaignRow
	for rows.Next() {
		var c CampaignRow
		if err := rows.Scan(&c.ID, &c.Name, &c.Channel, &c.Message, &c.Status, &c.SourceIntent, &c.Definition, &c.LaunchedAt, &c.Recipients); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetCampaign(ctx context.Context, id string) (*CampaignRow, error) {
	var c CampaignRow
	err := s.DB.QueryRowContext(ctx, `
		SELECT g.id, g.name, g.channel, g.message, g.status, g.source_intent, g.definition_snapshot, g.launched_at,
		       (SELECT COUNT(*) FROM communications m WHERE m.campaign_id=g.id)
		FROM campaigns g WHERE g.id=$1`, id).
		Scan(&c.ID, &c.Name, &c.Channel, &c.Message, &c.Status, &c.SourceIntent, &c.Definition, &c.LaunchedAt, &c.Recipients)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ---------- dispatch claim (the Postgres-outbox queue) ----------

type ClaimedComm struct {
	ID         string
	CampaignID string
	CustomerID string
	Channel    string
	Recipient  string
	Attempt    int
}

// Claim atomically leases a batch of due communications. SKIP LOCKED lets
// concurrent workers (even across instances) pull disjoint batches.
// attempt_count++ and a pushed next_attempt_at happen at claim time, so a
// crashed worker's rows simply become due again later (lease + backoff in
// one move).
func (s *Store) Claim(ctx context.Context, batch int, baseBackoffSec float64) ([]ClaimedComm, error) {
	rows, err := s.DB.QueryContext(ctx, `
		WITH due AS (
			SELECT id FROM communications
			WHERE current_status = 'queued' AND next_attempt_at <= now()
			ORDER BY next_attempt_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE communications m
		SET attempt_count = m.attempt_count + 1,
		    next_attempt_at = now() + make_interval(secs => $2 * power(2, m.attempt_count))
		FROM due WHERE m.id = due.id
		RETURNING m.id, m.campaign_id, m.customer_id, m.channel, m.recipient, m.attempt_count`,
		batch, baseBackoffSec)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClaimedComm
	for rows.Next() {
		var c ClaimedComm
		if err := rows.Scan(&c.ID, &c.CampaignID, &c.CustomerID, &c.Channel, &c.Recipient, &c.Attempt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecordEvent appends to the log and advances current_status, atomically.
//   * INSERT ... ON CONFLICT (event_id) DO NOTHING — duplicate callbacks no-op
//   * rank-guarded UPDATE — out-of-order callbacks can't regress status
// The log is the source of truth; current_status is a rebuildable cache.
func (s *Store) RecordEvent(ctx context.Context, commID, eventID, eventType string, occurredAt time.Time, lastError string) (inserted bool, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO communication_events (communication_id, event_id, event_type, occurred_at)
		VALUES ($1,$2,$3,$4) ON CONFLICT (event_id) DO NOTHING`,
		commID, eventID, eventType, occurredAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if _, err = tx.ExecContext(ctx, `
		UPDATE communications
		SET current_status = $1, last_error = CASE WHEN $1='failed' THEN $3 ELSE last_error END
		WHERE id = $2 AND status_rank($1) > status_rank(current_status)`,
		eventType, commID, lastError); err != nil {
		return false, err
	}
	return n > 0, tx.Commit()
}

// MarkFailed dead-letters a communication after exhausted retries or a
// permanent rejection, recording an internal failed event so the log stays
// complete (rebuildable-from-log holds for failures too).
func (s *Store) MarkFailed(ctx context.Context, commID, eventID, reason string) error {
	_, err := s.RecordEvent(ctx, commID, eventID, "failed", time.Now().UTC(), reason)
	return err
}

// ---------- personalization data ----------

type CustomerProfile struct {
	Name             string
	City             string
	FavoriteProduct  string
	FavoriteCategory string
}

// Profile loads what the token renderer needs. Favorite = most distinct
// orders, spend tiebreak. Empty favorites are handled by the renderer's
// fallback chain, never sent raw.
func (s *Store) Profile(ctx context.Context, customerID string) (CustomerProfile, error) {
	var p CustomerProfile
	if err := s.DB.QueryRowContext(ctx, `SELECT name, city FROM customers WHERE id=$1`, customerID).Scan(&p.Name, &p.City); err != nil {
		return p, err
	}
	_ = s.DB.QueryRowContext(ctx, `
		SELECT p.name FROM order_items oi
		JOIN orders o ON o.id=oi.order_id JOIN products p ON p.id=oi.product_id
		WHERE o.customer_id=$1
		GROUP BY p.name ORDER BY COUNT(DISTINCT o.id) DESC, SUM(oi.quantity*oi.unit_price) DESC LIMIT 1`,
		customerID).Scan(&p.FavoriteProduct)
	_ = s.DB.QueryRowContext(ctx, `
		SELECT p.category FROM order_items oi
		JOIN orders o ON o.id=oi.order_id JOIN products p ON p.id=oi.product_id
		WHERE o.customer_id=$1
		GROUP BY p.category ORDER BY COUNT(DISTINCT o.id) DESC, SUM(oi.quantity*oi.unit_price) DESC LIMIT 1`,
		customerID).Scan(&p.FavoriteCategory)
	return p, nil
}

func (s *Store) CampaignMessage(ctx context.Context, campaignID string) (string, error) {
	var m string
	err := s.DB.QueryRowContext(ctx, `SELECT message FROM campaigns WHERE id=$1`, campaignID).Scan(&m)
	return m, err
}

// ---------- stats ----------

type Stats struct {
	Recipients int            `json:"recipients"`
	ByStatus   map[string]int `json:"by_status"`
	Funnel     map[string]int `json:"funnel"` // cumulative: delivered includes opened/read/clicked
	Failed     int            `json:"failed"`
	Pending    int            `json:"pending"` // queued + retrying
	Rates      map[string]float64 `json:"rates"`
	OrdersAttributed  int     `json:"orders_attributed"`
	RevenueAttributed float64 `json:"revenue_attributed"`
	DerivedStatus     string  `json:"derived_status"`
}

var rankOrder = []string{"sent", "delivered", "opened", "read", "clicked"}
var rankVal = map[string]int{"queued": 0, "sent": 10, "delivered": 20, "failed": 25, "opened": 30, "read": 40, "clicked": 50}

// CampaignStats: the whole funnel from ONE grouped query — possible because
// status is monotonic, so "delivered" = everyone at rank >= delivered.
func (s *Store) CampaignStats(ctx context.Context, campaignID string, launchedAt time.Time) (Stats, error) {
	st := Stats{ByStatus: map[string]int{}, Funnel: map[string]int{}, Rates: map[string]float64{}}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT current_status, COUNT(*) FROM communications WHERE campaign_id=$1 GROUP BY current_status`, campaignID)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return st, err
		}
		st.ByStatus[status] = n
		st.Recipients += n
	}
	st.Failed = st.ByStatus["failed"]
	st.Pending = st.ByStatus["queued"]
	for _, stage := range rankOrder {
		total := 0
		for status, n := range st.ByStatus {
			if status != "failed" && rankVal[status] >= rankVal[stage] {
				total += n
			}
		}
		st.Funnel[stage] = total
	}
	if st.Recipients > 0 {
		st.Rates["delivery"] = pct(st.Funnel["delivered"], st.Recipients)
		st.Rates["open"] = pct(st.Funnel["opened"], st.Recipients)
		st.Rates["click"] = pct(st.Funnel["clicked"], st.Recipients)
		st.Rates["failure"] = pct(st.Failed, st.Recipients)
	}

	if err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(total_amount),0)::float8
		FROM orders WHERE attributed_campaign_id=$1`, campaignID).
		Scan(&st.OrdersAttributed, &st.RevenueAttributed); err != nil {
		return st, err
	}
	if st.Recipients > 0 {
		st.Rates["conversion"] = pct(st.OrdersAttributed, st.Recipients)
	}

	// Derived completion: sending while anything is queued or sent; completed
	// once everyone has settled OR the simulated-lifecycle window has passed
	// (delivered-but-never-opened is also an ending).
	switch {
	case st.Recipients == 0:
		st.DerivedStatus = "empty"
	case st.ByStatus["queued"] > 0 || st.ByStatus["sent"] > 0:
		if time.Since(launchedAt) > 15*time.Minute {
			st.DerivedStatus = "completed"
		} else {
			st.DerivedStatus = "sending"
		}
	default:
		st.DerivedStatus = "completed"
	}
	return st, nil
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) * 100.0 / float64(b)
}

// RecentEvents feeds the live event feed on the campaign page.
func (s *Store) RecentEvents(ctx context.Context, campaignID string, limit int) ([]map[string]interface{}, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT e.event_type, e.occurred_at, c.name
		FROM communication_events e
		JOIN communications m ON m.id = e.communication_id
		JOIN customers c ON c.id = m.customer_id
		WHERE m.campaign_id = $1
		ORDER BY e.received_at DESC LIMIT $2`, campaignID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var typ, name string
		var at time.Time
		if err := rows.Scan(&typ, &at, &name); err != nil {
			return nil, err
		}
		out = append(out, map[string]interface{}{"event_type": typ, "occurred_at": at, "customer": name})
	}
	return out, rows.Err()
}
