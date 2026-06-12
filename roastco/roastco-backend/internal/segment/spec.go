// Package segment defines the structured segment specification — the only
// interface between the AI and the database. The LLM authors a Spec; this
// package validates it against a closed whitelist and compiles it to
// parameterized SQL. A field or operator the model invents has no mapping
// and cannot compile: injection-safety and hallucination-safety come from
// the same mechanism.
package segment

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Rule struct {
	Field      string      `json:"field"`
	Op         string      `json:"op"`
	Value      interface{} `json:"value"`
	WithinDays int         `json:"within_days,omitempty"`
}

type Spec struct {
	Match string `json:"match"` // "all" | "any"
	Rules []Rule `json:"rules"`
}

// Categories is the live product taxonomy the AI must map intent onto.
var Categories = []string{"beans", "ground", "equipment", "accessories"}

// Channels supported by the channel service.
var Channels = []string{"email", "sms", "whatsapp", "rcs"}

// fieldKind drives both validation and compilation.
type fieldKind int

const (
	kindNumeric fieldKind = iota // numeric customer scalar
	kindString                   // string customer scalar
	kindPurchase                 // purchase predicate over orders/items/products
)

type fieldDef struct {
	kind fieldKind
	ops  map[string]bool
}

var allowedFields = map[string]fieldDef{
	"total_spend":           {kindNumeric, ops(">", "<", ">=", "<=", "between")},
	"order_count":           {kindNumeric, ops(">", "<", ">=", "<=", "=", "between")},
	"days_since_last_order": {kindNumeric, ops(">", "<", ">=", "<=", "between")},
	"days_since_signup":     {kindNumeric, ops(">", "<", ">=", "<=", "between")},
	"city":                  {kindString, ops("=", "in")},
	"favorite_category":     {kindString, ops("=", "in")},
	"purchased_category":    {kindPurchase, ops("includes", "excludes")},
	"purchased_product":     {kindPurchase, ops("includes", "excludes")},
}

func ops(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// FieldCatalog is a human/LLM-readable description of the whitelist, used in
// the Grok system prompt and exposed at /api/meta/fields for the UI.
func FieldCatalog() []map[string]interface{} {
	out := []map[string]interface{}{
		{"field": "total_spend", "type": "number (₹ lifetime spend)", "ops": []string{">", "<", ">=", "<=", "between"}},
		{"field": "order_count", "type": "number (lifetime orders)", "ops": []string{">", "<", ">=", "<=", "=", "between"}},
		{"field": "days_since_last_order", "type": "number (days; customers with no orders never match)", "ops": []string{">", "<", ">=", "<=", "between"}},
		{"field": "days_since_signup", "type": "number (days)", "ops": []string{">", "<", ">=", "<=", "between"}},
		{"field": "city", "type": "string", "ops": []string{"=", "in"}},
		{"field": "favorite_category", "type": "one of " + strings.Join(Categories, "/") + " (most distinct orders, spend tiebreak)", "ops": []string{"=", "in"}},
		{"field": "purchased_category", "type": "one of " + strings.Join(Categories, "/") + "; optional within_days window", "ops": []string{"includes", "excludes"}},
		{"field": "purchased_product", "type": "product name substring; optional within_days window", "ops": []string{"includes", "excludes"}},
	}
	return out
}

func Parse(raw []byte) (Spec, error) {
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("segment definition is not valid JSON: %w", err)
	}
	return s, s.Validate()
}

// Validate enforces the whitelist. Errors are written to be surfaced to the
// marketer ("try rephrasing"), not to crash the request.
func (s Spec) Validate() error {
	if s.Match != "all" && s.Match != "any" {
		return fmt.Errorf(`match must be "all" or "any", got %q`, s.Match)
	}
	if len(s.Rules) == 0 {
		return fmt.Errorf("a segment needs at least one rule")
	}
	if len(s.Rules) > 12 {
		return fmt.Errorf("too many rules (max 12)")
	}
	for i, r := range s.Rules {
		def, ok := allowedFields[r.Field]
		if !ok {
			return fmt.Errorf("rule %d: unknown field %q", i+1, r.Field)
		}
		if !def.ops[r.Op] {
			return fmt.Errorf("rule %d: operator %q is not allowed for %q", i+1, r.Op, r.Field)
		}
		switch def.kind {
		case kindNumeric:
			if r.Op == "between" {
				if _, _, err := numPair(r.Value); err != nil {
					return fmt.Errorf("rule %d: %v", i+1, err)
				}
			} else if _, err := num(r.Value); err != nil {
				return fmt.Errorf("rule %d: %v", i+1, err)
			}
		case kindString:
			if r.Op == "in" {
				if _, err := strList(r.Value); err != nil {
					return fmt.Errorf("rule %d: %v", i+1, err)
				}
			} else if _, err := str(r.Value); err != nil {
				return fmt.Errorf("rule %d: %v", i+1, err)
			}
			if r.Field == "favorite_category" {
				if err := checkCategories(r.Value); err != nil {
					return fmt.Errorf("rule %d: %v", i+1, err)
				}
			}
		case kindPurchase:
			v, err := str(r.Value)
			if err != nil {
				return fmt.Errorf("rule %d: %v", i+1, err)
			}
			if r.Field == "purchased_category" && !isCategory(v) {
				return fmt.Errorf("rule %d: %q is not a product category (valid: %s)", i+1, v, strings.Join(Categories, ", "))
			}
			if r.WithinDays < 0 || r.WithinDays > 3650 {
				return fmt.Errorf("rule %d: within_days out of range", i+1)
			}
		}
	}
	return nil
}

func isCategory(v string) bool {
	for _, c := range Categories {
		if c == v {
			return true
		}
	}
	return false
}

func checkCategories(v interface{}) error {
	if s, err := str(v); err == nil {
		if !isCategory(s) {
			return fmt.Errorf("%q is not a product category", s)
		}
		return nil
	}
	list, err := strList(v)
	if err != nil {
		return err
	}
	for _, s := range list {
		if !isCategory(s) {
			return fmt.Errorf("%q is not a product category", s)
		}
	}
	return nil
}

func num(v interface{}) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case json.Number:
		f, err := n.Float64()
		return f, err
	}
	return 0, fmt.Errorf("expected a number, got %v", v)
}

func numPair(v interface{}) (float64, float64, error) {
	arr, ok := v.([]interface{})
	if !ok || len(arr) != 2 {
		return 0, 0, fmt.Errorf(`"between" needs a value of [low, high]`)
	}
	a, err := num(arr[0])
	if err != nil {
		return 0, 0, err
	}
	b, err := num(arr[1])
	if err != nil {
		return 0, 0, err
	}
	return a, b, nil
}

func str(v interface{}) (string, error) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("expected a non-empty string, got %v", v)
	}
	return strings.TrimSpace(s), nil
}

func strList(v interface{}) ([]string, error) {
	arr, ok := v.([]interface{})
	if !ok || len(arr) == 0 {
		return nil, fmt.Errorf(`"in" needs a non-empty list of strings`)
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, err := str(item)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
