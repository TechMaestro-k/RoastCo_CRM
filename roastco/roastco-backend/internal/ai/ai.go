// Package ai holds the three bounded AI jobs: intent → segment spec,
// message-template drafting (with channel suggestion), and stats narration.
// The AI authors; a deterministic engine executes. Nothing here ever touches
// the database or sends a message.
package ai

import (
	"context"
	"os"

	"github.com/roastco/backend/internal/segment"
)

// PlanResult is the structured output of the intent → audience job.
type PlanResult struct {
	Spec          segment.Spec `json:"spec"`
	SegmentName   string       `json:"segment_name"`
	Interpretation string      `json:"interpretation"` // one sentence: how the intent was read
}

// Draft is the structured output of the message job. Message is a TEMPLATE —
// the backend fills {{tokens}} per customer at dispatch (one LLM call per
// campaign, no PII shipped in bulk, deterministic fill).
type Draft struct {
	Message        string `json:"message"`
	Channel        string `json:"channel"`
	ChannelReason  string `json:"channel_reason"`
	CampaignName   string `json:"campaign_name"`
}

type Planner interface {
	PlanSegment(ctx context.Context, intent string) (PlanResult, error)
	DraftMessage(ctx context.Context, intent string, spec segment.Spec) (Draft, error)
	Narrate(ctx context.Context, statsSummary string) (string, error)
	Mode() string
}

// New picks the implementation from AI_MODE:
//   "grok" — live LLM via XAI_API_KEY: xAI Grok (xai-…) or Groq (gsk_…), auto-detected
//   "mock" — deterministic heuristic planner (tests, offline demo insurance)
// Default: grok if a key is present, otherwise mock.
func New() Planner {
	mode := os.Getenv("AI_MODE")
	key := os.Getenv("XAI_API_KEY")
	if mode == "grok" || (mode == "" && key != "") {
		return newGrok(key)
	}
	return newMock()
}

// Tokens available in message templates; fill logic lives in dispatch.
var TemplateTokens = []string{"{{first_name}}", "{{name}}", "{{favorite_product}}", "{{favorite_category}}", "{{city}}"}
