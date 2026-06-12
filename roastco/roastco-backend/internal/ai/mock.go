package ai

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/roastco/backend/internal/segment"
)

// mockPlanner is a deterministic heuristic planner. It exists for two
// reasons: (1) end-to-end tests run without network access to xAI, and
// (2) demo insurance — if Grok is down or slow during a live recording, the
// product still works (AI_MODE=mock). It produces the exact same JSON shapes
// as the Grok planner and goes through the same validator.
type mockPlanner struct{}

func newMock() *mockPlanner { return &mockPlanner{} }

func (m *mockPlanner) Mode() string { return "mock" }

var (
	reDays    = regexp.MustCompile(`(\d+)\s*day`)
	reMonths  = regexp.MustCompile(`(\d+)\s*month`)
	reSpend   = regexp.MustCompile(`(?:spent|spend|spending)\D{0,20}?(\d[\d,]*)`)
	reOver    = regexp.MustCompile(`(?:over|above|more than|at least)\s*₹?\s*(\d[\d,]*)`)
)

var categoryWords = map[string]string{
	"bean": "beans", "beans": "beans", "whole": "beans",
	"ground": "ground",
	"grinder": "equipment", "equipment": "equipment", "brewer": "equipment",
	"kettle": "equipment", "machine": "equipment", "aeropress": "equipment",
	"chemex": "equipment", "v60": "equipment", "press": "equipment",
	"mug": "accessories", "filter": "accessories", "scale": "accessories",
	"accessor": "accessories", "canister": "accessories", "tamper": "accessories",
}

var knownCities = []string{"mumbai", "delhi", "bangalore", "bengaluru", "gurgaon", "pune", "hyderabad", "chennai", "jaipur", "kolkata", "noida"}

func (m *mockPlanner) PlanSegment(_ context.Context, intent string) (PlanResult, error) {
	t := strings.ToLower(intent)
	var rules []segment.Rule
	var notes []string

	// Recency: "haven't ordered in 60 days", "lapsed", "win back"
	lapsedIntent := strings.Contains(t, "haven't") || strings.Contains(t, "havent") ||
		strings.Contains(t, "lapsed") || strings.Contains(t, "win back") ||
		strings.Contains(t, "winback") || strings.Contains(t, "inactive") ||
		strings.Contains(t, "not ordered") || strings.Contains(t, "no order")
	if md := reDays.FindStringSubmatch(t); md != nil && lapsedIntent {
		n, _ := strconv.Atoi(md[1])
		rules = append(rules, segment.Rule{Field: "days_since_last_order", Op: ">", Value: float64(n)})
		notes = append(notes, fmt.Sprintf("no order in %d+ days", n))
	} else if lapsedIntent {
		rules = append(rules, segment.Rule{Field: "days_since_last_order", Op: ">", Value: float64(60)})
		notes = append(notes, "no order in 60+ days (default window)")
	}

	// Purchase window: "in the last N months"
	within := 0
	if mm := reMonths.FindStringSubmatch(t); mm != nil && strings.Contains(t, "last") {
		n, _ := strconv.Atoi(mm[1])
		within = n * 30
	}

	// Categories: words before a negation → includes; after → excludes.
	negIdx := negationIndex(t)
	seenInc, seenExc := map[string]bool{}, map[string]bool{}
	for word, cat := range categoryWords {
		idx := strings.Index(t, word)
		if idx < 0 {
			continue
		}
		if negIdx >= 0 && idx > negIdx {
			if !seenExc[cat] {
				rules = append(rules, segment.Rule{Field: "purchased_category", Op: "excludes", Value: cat})
				seenExc[cat] = true
				notes = append(notes, "never bought "+cat)
			}
		} else if !seenInc[cat] {
			r := segment.Rule{Field: "purchased_category", Op: "includes", Value: cat}
			if within > 0 {
				r.WithinDays = within
			}
			rules = append(rules, r)
			seenInc[cat] = true
			if within > 0 {
				notes = append(notes, fmt.Sprintf("bought %s in last %d days", cat, within))
			} else {
				notes = append(notes, "bought "+cat)
			}
		}
	}

	// Spend: "spent over 5000", "top customers", "vip"
	if ms := firstMatch(t, reOver, reSpend); ms != "" {
		v, _ := strconv.ParseFloat(strings.ReplaceAll(ms, ",", ""), 64)
		rules = append(rules, segment.Rule{Field: "total_spend", Op: ">", Value: v})
		notes = append(notes, fmt.Sprintf("spent over ₹%.0f", v))
	} else if strings.Contains(t, "vip") || strings.Contains(t, "top customer") || strings.Contains(t, "whale") || strings.Contains(t, "best customer") {
		rules = append(rules, segment.Rule{Field: "total_spend", Op: ">", Value: float64(8000)})
		notes = append(notes, "high spenders (₹8000+)")
	}

	// City
	for _, city := range knownCities {
		if strings.Contains(t, city) {
			c := city
			if c == "bengaluru" {
				c = "bangalore"
			}
			rules = append(rules, segment.Rule{Field: "city", Op: "=", Value: strings.Title(c)})
			notes = append(notes, "in "+strings.Title(c))
			break
		}
	}

	if len(rules) == 0 {
		// Nothing parsed: a safe, honest default the marketer can edit.
		rules = append(rules, segment.Rule{Field: "order_count", Op: ">=", Value: float64(1)})
		notes = append(notes, "all customers with at least one order (couldn't parse a narrower intent — edit the rules)")
	}

	spec := segment.Spec{Match: "all", Rules: rules}
	name := nameFromNotes(notes)
	return PlanResult{
		Spec:           spec,
		SegmentName:    name,
		Interpretation: "Reading the intent as: " + strings.Join(notes, "; ") + ".",
	}, spec.Validate()
}

func negationIndex(t string) int {
	best := -1
	for _, marker := range []string{"never", "but not", "without", "haven't bought", "didn't buy", "no ", "not bought", "excluding"} {
		if i := strings.Index(t, marker); i >= 0 && (best == -1 || i < best) {
			best = i
		}
	}
	return best
}

func firstMatch(t string, res ...*regexp.Regexp) string {
	for _, re := range res {
		if m := re.FindStringSubmatch(t); m != nil {
			return m[1]
		}
	}
	return ""
}

func nameFromNotes(notes []string) string {
	n := strings.Join(notes, " · ")
	if len(n) > 60 {
		n = n[:57] + "…"
	}
	return strings.ToUpper(n[:1]) + n[1:]
}

func (m *mockPlanner) DraftMessage(_ context.Context, intent string, spec segment.Spec) (Draft, error) {
	t := strings.ToLower(intent)
	offer := extractOffer(t)
	winback := strings.Contains(t, "win back") || strings.Contains(t, "lapsed") || strings.Contains(t, "haven't")

	msg := "Hi {{first_name}}, your {{favorite_product}} is calling — it's been a while since your last brew. "
	channel, reason := "email", "Default channel for rich content."
	name := "Roast & Co campaign"
	switch {
	case winback && offer != "":
		msg = "Hi {{first_name}}, we've missed you at Roast & Co ☕ Your {{favorite_product}} is waiting — here's " + offer + " on your next order."
		channel, reason = "whatsapp", "Win-back with an offer link performs best on WhatsApp's high open rates."
		name = "Win-back · " + offer
	case winback:
		msg = "Hi {{first_name}}, it's been a while! Your {{favorite_product}} misses you — come see what's fresh off the roaster."
		channel, reason = "whatsapp", "Re-engagement nudges land best on WhatsApp."
		name = "Win-back nudge"
	case offer != "":
		msg = "Hi {{first_name}}, a little something for you: " + offer + " on {{favorite_category}} at Roast & Co. Fresh roast, fresher deal."
		channel, reason = "whatsapp", "Offers with links get the strongest click-through on WhatsApp."
		name = "Offer · " + offer
	case strings.Contains(t, "new") || strings.Contains(t, "launch"):
		msg = "Hi {{first_name}}, something new just landed at Roast & Co — and given your love for {{favorite_category}}, we think you'll want first pour."
		channel, reason = "email", "Product announcements suit email's richer format."
		name = "New arrival announcement"
	}
	return Draft{Message: msg, Channel: channel, ChannelReason: reason, CampaignName: name}, nil
}

var reOffer = regexp.MustCompile(`(\d+)\s*%\s*(?:off)?`)

func extractOffer(t string) string {
	if m := reOffer.FindStringSubmatch(t); m != nil {
		return m[1] + "% off"
	}
	if strings.Contains(t, "free shipping") {
		return "free shipping"
	}
	return ""
}

func (m *mockPlanner) Narrate(_ context.Context, statsSummary string) (string, error) {
	return "Campaign summary (offline mode): " + statsSummary +
		" Consider repeating this audience with a follow-up offer for non-openers in 5–7 days.", nil
}
