// Package api is the CRM's HTTP surface. Handlers stay thin: parse, call the
// relevant package, encode. The receipt webhook is the inbound half of the
// callback loop and is secret-protected.
package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/roastco/backend/internal/ai"
	"github.com/roastco/backend/internal/attribution"
	"github.com/roastco/backend/internal/dispatch"
	"github.com/roastco/backend/internal/segment"
	"github.com/roastco/backend/internal/store"
	"github.com/roastco/backend/internal/wire"
)

type Server struct {
	S       *store.Store
	Planner ai.Planner
	Attr    *attribution.Attributor
	Secret  string
}

func New(s *store.Store, p ai.Planner, a *attribution.Attributor) *Server {
	secret := os.Getenv("CHANNEL_SHARED_SECRET")
	if secret == "" {
		secret = "dev-secret"
	}
	return &Server{S: s, Planner: p, Attr: a, Secret: secret}
}

func (srv *Server) Router() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", srv.health)
	mux.HandleFunc("GET /api/overview", srv.overview)
	mux.HandleFunc("GET /api/meta/fields", srv.metaFields)

	mux.HandleFunc("POST /api/ingest/customers", srv.ingestCustomers)
	mux.HandleFunc("POST /api/ingest/products", srv.ingestProducts)
	mux.HandleFunc("POST /api/ingest/orders", srv.ingestOrders)

	mux.HandleFunc("POST /api/segments/preview", srv.previewIntent)
	mux.HandleFunc("POST /api/segments/preview-spec", srv.previewSpec)
	mux.HandleFunc("POST /api/campaigns/draft", srv.draft)
	mux.HandleFunc("POST /api/campaigns", srv.launch)
	mux.HandleFunc("GET /api/campaigns", srv.listCampaigns)
	mux.HandleFunc("GET /api/campaigns/{id}", srv.getCampaign)
	mux.HandleFunc("GET /api/campaigns/{id}/stats", srv.stats)
	mux.HandleFunc("GET /api/campaigns/{id}/events", srv.events)
	mux.HandleFunc("GET /api/campaigns/{id}/narrate", srv.narrate)
	mux.HandleFunc("GET /api/campaigns/{id}/recipients", srv.recipients)
	mux.HandleFunc("GET /api/communications/{id}", srv.communication)

	mux.HandleFunc("POST /api/demo/simulate-order", srv.simulateOrder)
	mux.HandleFunc("POST /api/receipts", srv.receipts)

	return cors(mux)
}

// ---------- middleware & helpers ----------

func cors(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, origin := range strings.Split(os.Getenv("FRONTEND_ORIGIN"), ",") {
		origin = strings.TrimSpace(origin)
		if origin != "" {
			allowed[origin] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		switch {
		case len(allowed) == 0:
			w.Header().Set("Access-Control-Allow-Origin", "*")
		case allowed[origin]:
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Idempotency-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// ---------- health & meta ----------

func (srv *Server) health(w http.ResponseWriter, r *http.Request) {
	// Pings the DB so an external keep-alive (GitHub Action) hitting this
	// endpoint also keeps a free-tier Supabase project awake.
	if err := srv.S.DB.PingContext(r.Context()); err != nil {
		writeErr(w, 500, "database unreachable")
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "service": "crm", "ai_mode": srv.Planner.Mode()})
}

func (srv *Server) overview(w http.ResponseWriter, r *http.Request) {
	out, err := srv.S.Overview(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out["ai_mode"] = srv.Planner.Mode()
	out["attribution_window_days"] = srv.Attr.WindowDays()
	writeJSON(w, 200, out)
}

func (srv *Server) metaFields(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"fields":     segment.FieldCatalog(),
		"categories": segment.Categories,
		"channels":   segment.Channels,
		"tokens":     ai.TemplateTokens,
	})
}

// ---------- ingestion ----------

func (srv *Server) ingestCustomers(w http.ResponseWriter, r *http.Request) {
	var in []store.CustomerIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "body must be a JSON array of customers")
		return
	}
	n, err := srv.S.UpsertCustomers(r.Context(), in)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]int{"upserted": n})
}

func (srv *Server) ingestProducts(w http.ResponseWriter, r *http.Request) {
	var in []store.ProductIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "body must be a JSON array of products")
		return
	}
	n, err := srv.S.UpsertProducts(r.Context(), in)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]int{"upserted": n})
}

// ingestOrders is the double-duty endpoint: historical backfill AND the live
// heartbeat — every order ingested is immediately (re)attributed.
func (srv *Server) ingestOrders(w http.ResponseWriter, r *http.Request) {
	var in []store.OrderIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "body must be a JSON array of orders")
		return
	}
	// Historical backfill (seeding) can skip attribution: it runs before any
	// campaign exists, so every attribution query would find nothing. Live
	// orders (the default) always attribute. ?attribute=false turns it off.
	attribute := r.URL.Query().Get("attribute") != "false"
	attributed := 0
	for _, o := range in {
		orderID, _, _, err := srv.S.UpsertOrder(r.Context(), o)
		if err != nil {
			writeErr(w, 400, fmt.Sprintf("order %s: %v", o.ExternalID, err))
			return
		}
		if attribute {
			if campaign, err := srv.Attr.Attribute(r.Context(), orderID); err == nil && campaign != "" {
				attributed++
			}
		}
	}
	writeJSON(w, 200, map[string]int{"upserted": len(in), "attributed": attributed})
}

// ---------- AI: preview & draft ----------

func (srv *Server) previewIntent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Intent string `json:"intent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Intent) == "" {
		writeErr(w, 400, "intent is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	plan, err := srv.Planner.PlanSegment(ctx, in.Intent)
	if err != nil {
		writeErr(w, 422, "I couldn't build that segment — try rephrasing. ("+err.Error()+")")
		return
	}
	count, sample, err := srv.S.PreviewSegment(ctx, plan.Spec)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"spec":           plan.Spec,
		"segment_name":   plan.SegmentName,
		"interpretation": plan.Interpretation,
		"count":          count,
		"sample":         sample,
	})
}

// previewSpec is the edit loop: the marketer changed the rules; re-run them.
// No AI involved — deterministic execution of the edited spec.
func (srv *Server) previewSpec(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Definition segment.Spec `json:"definition"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "definition is required")
		return
	}
	count, sample, err := srv.S.PreviewSegment(r.Context(), in.Definition)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{"count": count, "sample": sample})
}

func (srv *Server) draft(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Intent     string       `json:"intent"`
		Definition segment.Spec `json:"definition"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "intent and definition are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	d, err := srv.Planner.DraftMessage(ctx, in.Intent, in.Definition)
	if err != nil {
		writeErr(w, 422, "I couldn't draft that message — try again. ("+err.Error()+")")
		return
	}
	writeJSON(w, 200, d)
}

// ---------- campaigns ----------

func (srv *Server) launch(w http.ResponseWriter, r *http.Request) {
	var in store.LaunchInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid campaign body")
		return
	}
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		in.IdempotencyKey = key
	}
	if in.Name == "" || in.Message == "" || !validChannel(in.Channel) {
		writeErr(w, 400, "name, message and a valid channel are required")
		return
	}
	id, recipients, replay, err := srv.S.LaunchCampaign(r.Context(), in)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	code := 201
	if replay {
		code = 200 // idempotent replay: same campaign, nothing re-created
	}
	writeJSON(w, code, map[string]interface{}{"campaign_id": id, "recipients": recipients, "replayed": replay})
}

func (srv *Server) listCampaigns(w http.ResponseWriter, r *http.Request) {
	list, err := srv.S.ListCampaigns(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, list)
}

func (srv *Server) getCampaign(w http.ResponseWriter, r *http.Request) {
	c, err := srv.S.GetCampaign(r.Context(), r.PathValue("id"))
	if err == sql.ErrNoRows {
		writeErr(w, 404, "campaign not found")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, c)
}

func (srv *Server) stats(w http.ResponseWriter, r *http.Request) {
	c, err := srv.S.GetCampaign(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "campaign not found")
		return
	}
	st, err := srv.S.CampaignStats(r.Context(), c.ID, c.LaunchedAt)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, st)
}

func (srv *Server) events(w http.ResponseWriter, r *http.Request) {
	ev, err := srv.S.RecentEvents(r.Context(), r.PathValue("id"), 25)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ev)
}

func (srv *Server) narrate(w http.ResponseWriter, r *http.Request) {
	c, err := srv.S.GetCampaign(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "campaign not found")
		return
	}
	st, err := srv.S.CampaignStats(r.Context(), c.ID, c.LaunchedAt)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	summary := fmt.Sprintf(
		`Campaign "%s" on %s: %d recipients, %d delivered (%.0f%%), %d clicked (%.0f%%), %d failed; %d attributed orders worth ₹%.0f (%.1f%% conversion).`,
		c.Name, c.Channel, st.Recipients, st.Funnel["delivered"], st.Rates["delivery"],
		st.Funnel["clicked"], st.Rates["click"], st.Failed,
		st.OrdersAttributed, st.RevenueAttributed, st.Rates["conversion"])
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	text, err := srv.Planner.Narrate(ctx, summary)
	if err != nil {
		text = summary // graceful degradation: raw summary beats an error
	}
	writeJSON(w, 200, map[string]string{"narration": text})
}

// ---------- message threads (the visible life of one message) ----------

// recipients lists a campaign's shoppers with their current message status —
// the left rail of the thread view. Most-engaged first so the interesting
// journeys surface on top.
func (srv *Server) recipients(w http.ResponseWriter, r *http.Request) {
	rows, err := srv.S.DB.QueryContext(r.Context(), `
		SELECT m.id, c.name, m.channel, m.current_status, m.attempt_count
		FROM communications m JOIN customers c ON c.id = m.customer_id
		WHERE m.campaign_id = $1
		ORDER BY status_rank(m.current_status) DESC, c.name
		LIMIT 30`, r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, name, channel, status string
		var attempts int
		if err := rows.Scan(&id, &name, &channel, &status, &attempts); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]interface{}{
			"communication_id": id, "customer": name, "channel": channel,
			"status": status, "attempts": attempts,
		})
	}
	writeJSON(w, 200, out)
}

// communication returns one message's full story: the exact rendered text the
// shopper received (same renderer the dispatcher uses) plus every lifecycle
// event in order — what the chat bubble and its ticks are drawn from.
func (srv *Server) communication(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var campaignID, customerID, customer, channel, recipient, status, lastError string
	var attempts int
	err := srv.S.DB.QueryRowContext(r.Context(), `
		SELECT m.campaign_id, m.customer_id, c.name, m.channel, m.recipient,
		       m.current_status, m.attempt_count, m.last_error
		FROM communications m JOIN customers c ON c.id = m.customer_id
		WHERE m.id = $1`, id).
		Scan(&campaignID, &customerID, &customer, &channel, &recipient, &status, &attempts, &lastError)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "communication not found")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	tpl, err := srv.S.CampaignMessage(r.Context(), campaignID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	profile, err := srv.S.Profile(r.Context(), customerID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	rows, err := srv.S.DB.QueryContext(r.Context(), `
		SELECT event_type, occurred_at FROM communication_events
		WHERE communication_id = $1 ORDER BY occurred_at, received_at`, id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	events := []map[string]interface{}{}
	for rows.Next() {
		var typ string
		var at time.Time
		if err := rows.Scan(&typ, &at); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		events = append(events, map[string]interface{}{"event_type": typ, "occurred_at": at})
	}

	writeJSON(w, 200, map[string]interface{}{
		"communication_id": id,
		"customer":         customer,
		"channel":          channel,
		"recipient":        recipient,
		"status":           status,
		"attempts":         attempts,
		"last_error":       lastError,
		"message":          dispatch.RenderTemplate(tpl, profile),
		"events":           events,
	})
}

// ---------- demo affordance ----------

// simulateOrder creates a small live order for a recent clicker (or any
// customer as fallback) through the SAME ingest+attribution path as real
// orders — it's a demo trigger, not a separate code path.
func (srv *Server) simulateOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CampaignID string `json:"campaign_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	ctx := r.Context()

	var customerExt, customerName string
	q := `
		SELECT c.external_id, c.name FROM communications m
		JOIN customers c ON c.id = m.customer_id
		WHERE m.current_status = 'clicked'`
	args := []interface{}{}
	if in.CampaignID != "" {
		q += ` AND m.campaign_id = $1`
		args = append(args, in.CampaignID)
	}
	q += ` ORDER BY random() LIMIT 1`
	err := srv.S.DB.QueryRowContext(ctx, q, args...).Scan(&customerExt, &customerName)
	if err != nil {
		// Fallback: any customer (the order will simply not attribute).
		if err := srv.S.DB.QueryRowContext(ctx,
			`SELECT external_id, name FROM customers ORDER BY random() LIMIT 1`).
			Scan(&customerExt, &customerName); err != nil {
			writeErr(w, 422, "no customers yet — seed data first")
			return
		}
	}

	var items []store.OrderItemIn
	rows, err := srv.S.DB.QueryContext(ctx, `SELECT external_id FROM products ORDER BY random() LIMIT 2`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for rows.Next() {
		var pid string
		_ = rows.Scan(&pid)
		items = append(items, store.OrderItemIn{ProductExternalID: pid, Quantity: 1})
	}
	rows.Close()
	if len(items) == 0 {
		writeErr(w, 422, "no products yet — seed data first")
		return
	}

	orderExt := "live-" + randHex(8)
	orderID, _, total, err := srv.S.UpsertOrder(ctx, store.OrderIn{
		ExternalID:         orderExt,
		CustomerExternalID: customerExt,
		Items:              items,
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	campaign, err := srv.Attr.Attribute(ctx, orderID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"order_external_id": orderExt,
		"customer":          customerName,
		"total":             total,
		"attributed_to":     campaign, // empty string = no qualifying touch
	})
}

// ---------- receipts (inbound half of the callback loop) ----------

func (srv *Server) receipts(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(wire.SecretHeader) != srv.Secret {
		writeErr(w, 401, "bad secret")
		return
	}
	var ev wire.Event
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil || ev.EventID == "" || ev.CommunicationID == "" {
		writeErr(w, 400, "invalid event")
		return
	}
	if !validEvent(ev.EventType) {
		writeErr(w, 400, "unknown event_type "+ev.EventType)
		return
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	inserted, err := srv.S.RecordEvent(r.Context(), ev.CommunicationID, ev.EventID, ev.EventType, ev.OccurredAt, "channel reported failure")
	if err != nil {
		// 5xx → the channel service retries; idempotency absorbs the replay.
		log.Printf("receipts: %v", err)
		writeErr(w, 500, "could not persist event")
		return
	}
	// 2xx only after a durable write. Duplicates report deduped:true.
	writeJSON(w, 200, map[string]bool{"ok": true, "deduped": !inserted})
}

func validChannel(c string) bool {
	for _, ch := range segment.Channels {
		if ch == c {
			return true
		}
	}
	return false
}

func validEvent(t string) bool {
	switch t {
	case "sent", "delivered", "opened", "read", "clicked", "failed":
		return true
	}
	return false
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
