package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/roastco/backend/internal/segment"
)

// grokPlanner calls an OpenAI-compatible chat completions endpoint with
// structured outputs. Two providers are supported and auto-detected from the
// key prefix: xAI Grok (keys start with "xai-") and Groq (keys start with
// "gsk_"). Either way, every response passes through segment.Validate —
// the whitelist, not the model, is the trust boundary.
type grokPlanner struct {
	key      string
	model    string
	base     string
	provider string
	client   *http.Client
}

func newGrok(key string) *grokPlanner {
	provider, base, defModel := "xai", "https://api.x.ai/v1", "grok-4.1-fast"
	if strings.HasPrefix(key, "gsk_") {
		// Groq (groq.com) — not Grok. Same wire format, different host.
		provider, base, defModel = "groq", "https://api.groq.com/openai/v1", "llama-3.3-70b-versatile"
	}
	model := os.Getenv("GROK_MODEL")
	// A grok-* model name with a Groq key is a config mix-up; use Groq's default.
	if model == "" || (provider == "groq" && strings.HasPrefix(model, "grok")) {
		model = defModel
	}
	if v := os.Getenv("XAI_BASE_URL"); v != "" {
		base = v
	}
	return &grokPlanner{key: key, model: model, base: base, provider: provider, client: &http.Client{Timeout: 45 * time.Second}}
}

func (g *grokPlanner) Mode() string { return g.provider + ":" + g.model }

// ---- request/response shapes (OpenAI-compatible) ----

type chatReq struct {
	Model          string      `json:"model"`
	Messages       []chatMsg   `json:"messages"`
	ResponseFormat interface{} `json:"response_format,omitempty"`
	Temperature    float64     `json:"temperature"`
}
type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// format picks the structured-output mode the provider actually supports.
// xAI Grok honours strict json_schema; Groq's Llama models only support
// json_object. Either way the response is vetted by segment.Validate, so
// json_object (looser) is perfectly safe — the model is asked for JSON and
// the whitelist rejects anything malformed or out-of-bounds.
func (g *grokPlanner) format(name string, schema map[string]interface{}) interface{} {
	if g.provider == "groq" {
		return map[string]string{"type": "json_object"}
	}
	return map[string]interface{}{
		"type": "json_schema",
		"json_schema": map[string]interface{}{
			"name":   name,
			"strict": true,
			"schema": schema,
		},
	}
}

func (g *grokPlanner) complete(ctx context.Context, system, user string, format interface{}) (string, error) {
	out, err := g.completeOnce(ctx, system, user, format)
	// Some models reject strict json_schema. Downgrade once to json_object —
	// safe, because the whitelist validator vets the content either way.
	if err != nil && format != nil && mentionsSchemaRejection(err.Error()) {
		out, err = g.completeOnce(ctx, system, user, map[string]string{"type": "json_object"})
	}
	return out, err
}

// mentionsSchemaRejection matches the various ways providers say "I don't
// support json_schema" — with an underscore, a space, or a backtick.
func mentionsSchemaRejection(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "response_format") ||
		strings.Contains(m, "response format") ||
		strings.Contains(m, "json_schema")
}

func (g *grokPlanner) completeOnce(ctx context.Context, system, user string, format interface{}) (string, error) {
	body, _ := json.Marshal(chatReq{
		Model:          g.model,
		Messages:       []chatMsg{{Role: "system", Content: system}, {Role: "user", Content: user}},
		ResponseFormat: format,
		Temperature:    0.2,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", g.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.key)
	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("grok request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out chatResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("grok returned non-JSON (%d): %s", resp.StatusCode, trim(string(raw), 200))
	}
	if out.Error != nil {
		return "", fmt.Errorf("grok error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("grok returned no choices (%d)", resp.StatusCode)
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// ---- job 1: intent → segment spec ----

var specSchema = map[string]interface{}{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"match", "rules", "segment_name", "interpretation"},
	"properties": map[string]interface{}{
		"match":          map[string]interface{}{"type": "string", "enum": []string{"all", "any"}},
		"segment_name":   map[string]interface{}{"type": "string"},
		"interpretation": map[string]interface{}{"type": "string"},
		"rules": map[string]interface{}{
			"type": "array",
			"items": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"field", "op", "value"},
				"properties": map[string]interface{}{
					"field":       map[string]interface{}{"type": "string"},
					"op":          map[string]interface{}{"type": "string"},
					"value":       map[string]interface{}{}, // number | string | array
					"within_days": map[string]interface{}{"type": "integer"},
				},
			},
		},
	},
}

func segmentSystemPrompt() string {
	cat, _ := json.Marshal(segment.FieldCatalog())
	return `You translate a coffee-brand marketer's plain-English audience intent into a structured segment spec for "Roast & Co" (specialty coffee: beans, ground, equipment, accessories; prices in ₹).
Output ONLY the JSON object. Use ONLY these fields and operators (anything else is rejected):
` + string(cat) + `
Notes:
- "grinder", "brewer", "kettle", "machine" → category "equipment". "mug", "filter", "scale" → "accessories".
- "lapsed", "haven't ordered", "win back" → days_since_last_order.
- "last N months" on a purchase → within_days = N*30 on that purchase rule.
- "bought X but never Y" → one includes rule + one excludes rule.
- segment_name: short, marketer-friendly (e.g. "Lapsed bean buyers · no grinder").
- interpretation: one sentence stating how you read the intent, so the marketer can verify.
Example intent: "win back customers who bought beans in the last 6 months but never bought a grinder, and haven't ordered in 60 days"
Example output: {"match":"all","rules":[{"field":"purchased_category","op":"includes","value":"beans","within_days":180},{"field":"purchased_category","op":"excludes","value":"equipment"},{"field":"days_since_last_order","op":">","value":60}],"segment_name":"Lapsed bean buyers · no grinder","interpretation":"Customers who bought beans in the last 180 days, never bought equipment, and haven't ordered in over 60 days."}`
}

func (g *grokPlanner) PlanSegment(ctx context.Context, intent string) (PlanResult, error) {
	out, err := g.complete(ctx, segmentSystemPrompt(), intent, g.format("segment_plan", specSchema))
	if err != nil {
		return PlanResult{}, err
	}
	var r struct {
		Match          string         `json:"match"`
		Rules          []segment.Rule `json:"rules"`
		SegmentName    string         `json:"segment_name"`
		Interpretation string         `json:"interpretation"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return PlanResult{}, fmt.Errorf("could not parse the AI's segment spec: %w", err)
	}
	res := PlanResult{Spec: segment.Spec{Match: r.Match, Rules: r.Rules}, SegmentName: r.SegmentName, Interpretation: r.Interpretation}
	return res, res.Spec.Validate() // the whitelist has the final word
}

// ---- job 2: message template + channel suggestion ----

var draftSchema = map[string]interface{}{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"message", "channel", "channel_reason", "campaign_name"},
	"properties": map[string]interface{}{
		"message":        map[string]interface{}{"type": "string"},
		"channel":        map[string]interface{}{"type": "string", "enum": segment.Channels},
		"channel_reason": map[string]interface{}{"type": "string"},
		"campaign_name":  map[string]interface{}{"type": "string"},
	},
}

func (g *grokPlanner) DraftMessage(ctx context.Context, intent string, spec segment.Spec) (Draft, error) {
	specJSON, _ := json.Marshal(spec)
	system := `You draft ONE personalised message TEMPLATE for a Roast & Co (specialty coffee, India, prices in ₹) campaign, plus a channel suggestion.
Rules:
- The message is a template sent to many shoppers. Personalise ONLY with these tokens: {{first_name}}, {{name}}, {{favorite_product}}, {{favorite_category}}, {{city}}. Never invent other tokens.
- Warm, specific, coffee-brand voice. 1-3 short sentences. Include any offer stated in the intent verbatim.
- channel: email for long/rich content, whatsapp for offers with links/high engagement, sms for short urgent nudges, rcs for rich mobile.
- channel_reason: one sentence. campaign_name: short and descriptive.
Output ONLY the JSON object.`
	user := fmt.Sprintf("Marketer intent: %s\nAudience spec: %s", intent, specJSON)
	out, err := g.complete(ctx, system, user, g.format("message_draft", draftSchema))
	if err != nil {
		return Draft{}, err
	}
	var d Draft
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return Draft{}, fmt.Errorf("could not parse the AI's draft: %w", err)
	}
	if !validChannel(d.Channel) {
		d.Channel = "email"
	}
	return d, nil
}

// ---- job 3 (optional flourish): narrate campaign stats ----

func (g *grokPlanner) Narrate(ctx context.Context, statsSummary string) (string, error) {
	system := "You are a marketing analyst for Roast & Co. In 2-3 plain sentences, summarise this campaign's performance for the marketer and suggest one concrete next step. No headings, no bullets, currency in ₹."
	return g.complete(ctx, system, statsSummary, nil)
}

func validChannel(c string) bool {
	for _, ch := range segment.Channels {
		if ch == c {
			return true
		}
	}
	return false
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
