// Package wire defines the HTTP contracts shared by the CRM backend and the
// channel service. The two services communicate only through these shapes.
package wire

import "time"

// SendRequest is what the CRM posts to the channel service's /send endpoint.
// CommunicationID doubles as the send-side idempotency key: the channel
// service ignores a repeat send for an ID it has already accepted.
// CallbackURL tells the channel where to deliver receipts — the channel
// service holds no configuration about the CRM at all.
type SendRequest struct {
	CommunicationID string `json:"communication_id"`
	Channel         string `json:"channel"`
	Recipient       string `json:"recipient"`
	Message         string `json:"message"`
	CallbackURL     string `json:"callback_url"`
}

// Event is one delivery-lifecycle callback from the channel service to the
// CRM's /receipts endpoint. EventID is unique per event and is the CRM's
// dedup key (UNIQUE constraint in the event log).
type Event struct {
	EventID         string    `json:"event_id"`
	CommunicationID string    `json:"communication_id"`
	EventType       string    `json:"event_type"` // sent|delivered|opened|read|clicked|failed
	OccurredAt      time.Time `json:"occurred_at"`
}

// SecretHeader carries the shared secret that authenticates both directions
// of the loop (CRM→/send and channel→/receipts).
const SecretHeader = "X-Channel-Secret"

// Statuses in lifecycle order. Rank logic lives in SQL (status_rank) so the
// guard against out-of-order regressions is enforced at the database.
var Statuses = []string{"queued", "sent", "delivered", "opened", "read", "clicked", "failed"}
