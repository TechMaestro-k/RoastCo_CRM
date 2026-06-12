// Package dispatch is the delivery engine: a bounded pool of workers pulling
// from the Postgres-backed queue (SKIP LOCKED claim) and calling the channel
// service. Bounded workers + bounded DB pool are the two independent guards
// that cap load no matter how big the campaign is.
package dispatch

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/roastco/backend/internal/store"
	"github.com/roastco/backend/internal/wire"
)

type Dispatcher struct {
	s           *store.Store
	channelURL  string
	callbackURL string
	secret      string
	workers     int
	batch       int
	maxAttempts int
	baseBackoff float64
	pollIdle    time.Duration
	client      *http.Client

	mu        sync.RWMutex
	templates map[string]string // campaign_id → message template (cache)
}

func New(s *store.Store) *Dispatcher {
	d := &Dispatcher{
		s:           s,
		channelURL:  envStr("CHANNEL_URL", "http://127.0.0.1:8081"),
		callbackURL: envStr("CALLBACK_URL", "http://127.0.0.1:8080/api/receipts"),
		secret:      envStr("CHANNEL_SHARED_SECRET", "dev-secret"),
		workers:     envInt("WORKER_COUNT", 12),
		batch:       envInt("DISPATCH_BATCH", 20),
		maxAttempts: envInt("DISPATCH_MAX_ATTEMPTS", 5),
		baseBackoff: envFloat("DISPATCH_BASE_BACKOFF_SEC", 5),
		pollIdle:    time.Duration(envInt("DISPATCH_IDLE_POLL_MS", 500)) * time.Millisecond,
		client:      &http.Client{Timeout: 10 * time.Second},
		templates:   map[string]string{},
	}
	return d
}

// Run starts the worker pool. Each worker loops: claim a batch → send each →
// sleep briefly when idle. The pace of the system is set by the network
// calls, not by database polling.
func (d *Dispatcher) Run(ctx context.Context) {
	log.Printf("dispatch: %d workers, batch %d, max %d attempts, base backoff %.1fs → %s",
		d.workers, d.batch, d.maxAttempts, d.baseBackoff, d.channelURL)
	for i := 0; i < d.workers; i++ {
		go d.worker(ctx, i)
	}
}

func (d *Dispatcher) worker(ctx context.Context, id int) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		claimed, err := d.s.Claim(ctx, d.batch, d.baseBackoff)
		if err != nil {
			log.Printf("dispatch[%d]: claim error: %v", id, err)
			time.Sleep(time.Second)
			continue
		}
		if len(claimed) == 0 {
			time.Sleep(d.pollIdle)
			continue
		}
		for _, c := range claimed {
			d.sendOne(ctx, c)
		}
	}
}

func (d *Dispatcher) sendOne(ctx context.Context, c store.ClaimedComm) {
	// Exhausted retries → dead-letter with an internal failed event so the
	// log stays complete.
	if c.Attempt > d.maxAttempts {
		_ = d.s.MarkFailed(ctx, c.ID, newEventID("dlq"), fmt.Sprintf("dead-lettered after %d attempts", c.Attempt-1))
		return
	}
	if strings.TrimSpace(c.Recipient) == "" {
		_ = d.s.MarkFailed(ctx, c.ID, newEventID("perm"), "no recipient contact for channel "+c.Channel)
		return
	}

	msg, err := d.render(ctx, c)
	if err != nil {
		log.Printf("dispatch: render %s: %v", c.ID, err)
		return // transient: row retries via its pushed next_attempt_at
	}

	body, _ := json.Marshal(wire.SendRequest{
		CommunicationID: c.ID,
		Channel:         c.Channel,
		Recipient:       c.Recipient,
		Message:         msg,
		CallbackURL:     d.callbackURL,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", d.channelURL+"/send", bytes.NewReader(body))
	if err != nil {
		// Misconfigured CHANNEL_URL (unparseable). Permanent for this config:
		// log loudly; rows stay queued and recover as soon as config is fixed.
		log.Printf("dispatch: BAD CHANNEL_URL %q: %v", d.channelURL, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(wire.SecretHeader, d.secret)

	resp, err := d.client.Do(req)
	if err != nil {
		// Transient (channel down, timeout): leave queued; the claim already
		// pushed next_attempt_at with exponential backoff.
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// Ack → record 'sent' as a real event so the log alone can rebuild
		// every status (no status lives outside the log).
		if _, err := d.s.RecordEvent(ctx, c.ID, newEventID("snt"), "sent", time.Now().UTC(), ""); err != nil {
			log.Printf("dispatch: record sent %s: %v", c.ID, err)
		}
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// Permanent rejection: no retry.
		_ = d.s.MarkFailed(ctx, c.ID, newEventID("rej"), fmt.Sprintf("channel rejected send (%d)", resp.StatusCode))
	default:
		// 5xx transient: retry later (already scheduled).
	}
}

// render fills the campaign template per customer. Deterministic, reviewable,
// with a fallback chain so an empty favorite never reaches a shopper.
func (d *Dispatcher) render(ctx context.Context, c store.ClaimedComm) (string, error) {
	d.mu.RLock()
	tpl, ok := d.templates[c.CampaignID]
	d.mu.RUnlock()
	if !ok {
		var err error
		tpl, err = d.s.CampaignMessage(ctx, c.CampaignID)
		if err != nil {
			return "", err
		}
		d.mu.Lock()
		d.templates[c.CampaignID] = tpl
		d.mu.Unlock()
	}
	p, err := d.s.Profile(ctx, c.CustomerID)
	if err != nil {
		return "", err
	}
	return RenderTemplate(tpl, p), nil
}

// RenderTemplate fills {{tokens}} from a customer profile. Exported so the
// API's message-thread view shows exactly what the dispatcher sends — one
// renderer, no drift between what's displayed and what's delivered.
func RenderTemplate(tpl string, p store.CustomerProfile) string {
	first := strings.Fields(p.Name)
	firstName := p.Name
	if len(first) > 0 {
		firstName = first[0]
	}
	favProduct := p.FavoriteProduct
	if favProduct == "" {
		favProduct = p.FavoriteCategory // fallback 1
	}
	if favProduct == "" {
		favProduct = "your next bag" // fallback 2: generic, never blank
	}
	favCategory := p.FavoriteCategory
	if favCategory == "" {
		favCategory = "coffee"
	}
	r := strings.NewReplacer(
		"{{first_name}}", firstName,
		"{{name}}", p.Name,
		"{{favorite_product}}", favProduct,
		"{{favorite_category}}", favCategory,
		"{{city}}", orDefault(p.City, "your city"),
	)
	return r.Replace(tpl)
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func newEventID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

func envStr(k, def string) string {
	// Trim: env values pasted into deploy dashboards often carry stray
	// whitespace, and a leading space makes a URL unparseable.
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
