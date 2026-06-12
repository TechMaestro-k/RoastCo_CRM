package segment

import (
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// Compile turns a validated Spec into a parameterized WHERE clause over
// customers c. Values are ALWAYS bound parameters ($n) — never concatenated —
// so the LLM's output can never reach the database as executable text.
// startArg is the first free placeholder number (callers may already hold $1..).
func Compile(s Spec, startArg int) (where string, args []interface{}, err error) {
	if err := s.Validate(); err != nil {
		return "", nil, err
	}
	next := startArg
	add := func(v interface{}) string {
		args = append(args, v)
		p := fmt.Sprintf("$%d", next)
		next++
		return p
	}

	var parts []string
	for _, r := range s.Rules {
		var frag string
		switch r.Field {
		case "total_spend":
			expr := "(SELECT COALESCE(SUM(o.total_amount),0) FROM orders o WHERE o.customer_id = c.id)"
			frag, err = numericFrag(expr, r, add)
		case "order_count":
			expr := "(SELECT COUNT(*) FROM orders o WHERE o.customer_id = c.id)"
			frag, err = numericFrag(expr, r, add)
		case "days_since_last_order":
			// NULL for customers with no orders → comparison is NULL → excluded.
			expr := "EXTRACT(epoch FROM now() - (SELECT MAX(o.ordered_at) FROM orders o WHERE o.customer_id = c.id)) / 86400.0"
			frag, err = numericFrag("("+expr+")", r, add)
		case "days_since_signup":
			expr := "(EXTRACT(epoch FROM now() - c.created_at) / 86400.0)"
			frag, err = numericFrag(expr, r, add)
		case "city":
			frag, err = stringFrag("c.city", r, add)
		case "favorite_category":
			expr := `(SELECT p.category FROM order_items oi
			           JOIN orders o ON o.id = oi.order_id
			           JOIN products p ON p.id = oi.product_id
			           WHERE o.customer_id = c.id
			           GROUP BY p.category
			           ORDER BY COUNT(DISTINCT o.id) DESC, SUM(oi.quantity * oi.unit_price) DESC
			           LIMIT 1)`
			frag, err = stringFrag(expr, r, add)
		case "purchased_category", "purchased_product":
			frag, err = purchaseFrag(r, add)
		default:
			err = fmt.Errorf("field %q has no compiler mapping", r.Field)
		}
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, "("+frag+")")
	}

	joiner := " AND "
	if s.Match == "any" {
		joiner = " OR "
	}
	return "(" + strings.Join(parts, joiner) + ")", args, nil
}

func numericFrag(expr string, r Rule, add func(interface{}) string) (string, error) {
	if r.Op == "between" {
		lo, hi, err := numPair(r.Value)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s BETWEEN %s AND %s", expr, add(lo), add(hi)), nil
	}
	v, err := num(r.Value)
	if err != nil {
		return "", err
	}
	switch r.Op { // operator comes from the whitelist, never from input text
	case ">", "<", ">=", "<=", "=":
		return fmt.Sprintf("%s %s %s", expr, r.Op, add(v)), nil
	}
	return "", fmt.Errorf("operator %q not compilable for numeric field", r.Op)
}

func stringFrag(expr string, r Rule, add func(interface{}) string) (string, error) {
	if r.Op == "in" {
		list, err := strList(r.Value)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("LOWER(%s) = ANY(%s)", expr, add(pq.Array(lower(list)))), nil
	}
	v, err := str(r.Value)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("LOWER(%s) = LOWER(%s)", expr, add(v)), nil
}

func purchaseFrag(r Rule, add func(interface{}) string) (string, error) {
	v, err := str(r.Value)
	if err != nil {
		return "", err
	}
	var match string
	if r.Field == "purchased_category" {
		match = "p.category = " + add(v)
	} else {
		match = "p.name ILIKE " + add("%"+v+"%")
	}
	window := ""
	if r.WithinDays > 0 {
		window = " AND o.ordered_at >= now() - make_interval(days => " + add(r.WithinDays) + "::int)"
	}
	sub := `SELECT 1 FROM orders o
	         JOIN order_items oi ON oi.order_id = o.id
	         JOIN products p ON p.id = oi.product_id
	         WHERE o.customer_id = c.id AND ` + match + window
	if r.Op == "excludes" {
		return "NOT EXISTS (" + sub + ")", nil
	}
	return "EXISTS (" + sub + ")", nil
}

func lower(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}
