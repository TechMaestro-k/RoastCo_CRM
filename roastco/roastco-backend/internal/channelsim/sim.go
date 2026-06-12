// Package channelsim is the stubbed channel provider. It is adversarial ON
// PURPOSE: random latency, a slice of transient 5xx failures, per-channel
// lifecycles with randomised delays (so callbacks arrive out of order), and
// deliberate duplicate callbacks — so the CRM's robustness is demonstrated,
// not assumed. State (dedup set, scheduled events) is in memory: a real
// provider persists this; the simulator accepts loss on restart as a stated
// tradeoff.
package channelsim

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"math"
	mrand "math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/roastco/backend/internal/wire"
)

type Sim struct {
	secret        string
	transientRate float64 // probability /send answers 503 (exercises CRM retries)
	failRate      float64 // probability the message lifecycle ends in 'failed'
	dupRate       float64 // probability any callback is sent twice (tests dedup)
	minDelay      time.Duration
	maxDelay      time.Duration
	client        *http.Client

	mu       sync.Mutex
	accepted map[string]bool // send-side idempotency: communication_id seen
	rng      *mrand.Rand
}

func New() *Sim {
	return &Sim{
		secret:        envStr("CHANNEL_SHARED_SECRET", "dev-secret"),
		transientRate: envFloat("CHANNEL_TRANSIENT_RATE", 0.03),
		failRate:      envFloat("CHANNEL_FAIL_RATE", 0.06),
		dupRate:       envFloat("CHANNEL_DUP_RATE", 0.12),
		minDelay:      time.Duration(envInt("CHANNEL_MIN_DELAY_MS", 800)) * time.Millisecond,
		maxDelay:      time.Duration(envInt("CHANNEL_MAX_DELAY_MS", 6000)) * time.Millisecond,
		client:        &http.Client{Timeout: 10 * time.Second},
		accepted:      map[string]bool{},
		rng:           mrand.New(mrand.NewSource(time.Now().UnixNano())),
	}
}

func (s *Sim) Router() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", s.handleSend)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"ok":true,"service":"channel"}`))
	})
	return mux
}

func (s *Sim) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(wire.SecretHeader) != s.secret {
		http.Error(w, `{"error":"bad secret"}`, http.StatusUnauthorized)
		return
	}
	var req wire.SendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CommunicationID == "" || req.CallbackURL == "" {
		http.Error(w, `{"error":"invalid send request"}`, http.StatusBadRequest)
		return
	}
	if req.Recipient == "" {
		http.Error(w, `{"error":"recipient required"}`, http.StatusUnprocessableEntity) // permanent
		return
	}

	// Simulated provider latency.
	time.Sleep(time.Duration(50+s.rng.Intn(250)) * time.Millisecond)

	// Transient infrastructure failure — the CRM should retry this send.
	if s.rng.Float64() < s.transientRate {
		http.Error(w, `{"error":"temporarily unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	// Send-side idempotency: a repeat send for an accepted ID is acknowledged
	// but NOT re-scheduled (the crashed-worker double-send is absorbed here).
	s.mu.Lock()
	already := s.accepted[req.CommunicationID]
	if !already {
		s.accepted[req.CommunicationID] = true
	}
	s.mu.Unlock()
	if !already {
		go s.lifecycle(req)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"accepted": true, "duplicate": already})
}

// lifecycle plays out what "happened" to the message, per channel:
//   email    : delivered → opened(70%) → clicked(60% of opened)
//   whatsapp : delivered → read(80%)   → clicked(55% of read)
//   rcs      : delivered → read(75%)   → clicked(50% of read)
//   sms      : delivered → clicked(25%)
// or, with failRate probability, a single 'failed' event instead.
// Click probabilities are generous on purpose — demo insurance, env-tunable.
func (s *Sim) lifecycle(req wire.SendRequest) {
	if s.chance(s.failRate) {
		s.emit(req, "failed", s.delay())
		return
	}
	t := s.delay()
	s.emit(req, "delivered", t)
	switch req.Channel {
	case "email":
		if s.chance(0.70) {
			t += s.delay()
			s.emit(req, "opened", t)
			if s.chance(0.60) {
				s.emit(req, "clicked", t+s.delay())
			}
		}
	case "whatsapp", "rcs":
		p := 0.80
		if req.Channel == "rcs" {
			p = 0.75
		}
		if s.chance(p) {
			t += s.delay()
			s.emit(req, "read", t)
			if s.chance(0.55) {
				s.emit(req, "clicked", t+s.delay())
			}
		}
	case "sms":
		if s.chance(0.25) {
			s.emit(req, "clicked", t+s.delay())
		}
	}
}

// emit schedules a callback after `after`, with retry-on-failure and the
// deliberate duplicate injection.
func (s *Sim) emit(req wire.SendRequest, eventType string, after time.Duration) {
	ev := wire.Event{
		EventID:         newID(),
		CommunicationID: req.CommunicationID,
		EventType:       eventType,
	}
	go func() {
		time.Sleep(after)
		ev.OccurredAt = time.Now().UTC()
		s.post(req.CallbackURL, ev)
		if s.chance(s.dupRate) {
			// Same event_id, posted again: the CRM's UNIQUE constraint should
			// make this a silent no-op. This runs in normal operation, so every
			// campaign exercises dedup.
			time.Sleep(time.Duration(100+s.rng.Intn(800)) * time.Millisecond)
			s.post(req.CallbackURL, ev)
		}
	}()
}

// post delivers a callback with at-least-once semantics: up to 5 attempts
// with exponential backoff. (Which is exactly why the CRM's receipt handler
// must be idempotent.)
func (s *Sim) post(url string, ev wire.Event) {
	body, _ := json.Marshal(ev)
	for attempt := 0; attempt < 5; attempt++ {
		req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(wire.SecretHeader, s.secret)
		resp, err := s.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(time.Duration(math.Pow(2, float64(attempt))) * 500 * time.Millisecond)
	}
	log.Printf("channel: callback %s for %s undeliverable after retries", ev.EventType, ev.CommunicationID)
}

func (s *Sim) delay() time.Duration {
	span := s.maxDelay - s.minDelay
	if span <= 0 {
		return s.minDelay
	}
	s.mu.Lock()
	d := s.minDelay + time.Duration(s.rng.Int63n(int64(span)))
	s.mu.Unlock()
	return d
}

func (s *Sim) chance(p float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.Float64() < p
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "evt-" + hex.EncodeToString(b)
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
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
