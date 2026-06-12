// Package store is the only place SQL runs. Bounded connection pool +
// bounded workers are the two independent guards that keep load on Postgres
// capped regardless of campaign size.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"time"

	_ "github.com/lib/pq"
)

type Store struct{ DB *sql.DB }

func Open() (*Store, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://crm:crm@127.0.0.1:5432/roastco?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	max := envInt("DB_MAX_CONNS", 25)
	db.SetMaxOpenConns(max)
	db.SetMaxIdleConns(max / 2)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("database unreachable: %w", err)
	}
	return &Store{DB: db}, nil
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// Migrate applies the schema (idempotent: IF NOT EXISTS / OR REPLACE throughout).
func (s *Store) Migrate(ctx context.Context, sqlText string) error {
	_, err := s.DB.ExecContext(ctx, sqlText)
	return err
}

// ---------- ingestion (idempotent contract) ----------

type CustomerIn struct {
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	Phone      string `json:"phone"`
	City       string `json:"city"`
	CreatedAt  string `json:"created_at,omitempty"` // RFC3339, optional (seed sets signup dates)
}

func (s *Store) UpsertCustomers(ctx context.Context, in []CustomerIn) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO customers (external_id, name, email, phone, city, created_at)
		VALUES ($1,$2,$3,$4,$5, COALESCE(NULLIF($6,'')::timestamptz, now()))
		ON CONFLICT (external_id) DO UPDATE
		SET name=EXCLUDED.name, email=EXCLUDED.email, phone=EXCLUDED.phone, city=EXCLUDED.city`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, c := range in {
		if c.ExternalID == "" {
			return 0, fmt.Errorf("customer missing external_id")
		}
		if _, err := stmt.ExecContext(ctx, c.ExternalID, c.Name, c.Email, c.Phone, c.City, c.CreatedAt); err != nil {
			return 0, err
		}
	}
	return len(in), tx.Commit()
}

type ProductIn struct {
	ExternalID string  `json:"external_id"`
	Name       string  `json:"name"`
	Category   string  `json:"category"`
	Price      float64 `json:"price"`
}

func (s *Store) UpsertProducts(ctx context.Context, in []ProductIn) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO products (external_id, name, category, price) VALUES ($1,$2,$3,$4)
		ON CONFLICT (external_id) DO UPDATE
		SET name=EXCLUDED.name, category=EXCLUDED.category, price=EXCLUDED.price`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, p := range in {
		if _, err := stmt.ExecContext(ctx, p.ExternalID, p.Name, p.Category, p.Price); err != nil {
			return 0, err
		}
	}
	return len(in), tx.Commit()
}

type OrderItemIn struct {
	ProductExternalID string  `json:"product_external_id"`
	Quantity          int     `json:"quantity"`
	UnitPrice         float64 `json:"unit_price,omitempty"` // 0 → use catalog price
}

type OrderIn struct {
	ExternalID         string        `json:"external_id"`
	CustomerExternalID string        `json:"customer_external_id"`
	OrderedAt          string        `json:"ordered_at"` // RFC3339; empty → now (live orders)
	Items              []OrderItemIn `json:"items"`
}

// UpsertOrder ingests one order idempotently (re-ingest replaces items and
// re-evaluates attribution — it sets a column, never increments a counter).
// Returns the order id and customer id so the caller can run attribution.
func (s *Store) UpsertOrder(ctx context.Context, o OrderIn) (orderID, customerID string, total float64, err error) {
	if o.ExternalID == "" || o.CustomerExternalID == "" || len(o.Items) == 0 {
		return "", "", 0, fmt.Errorf("order needs external_id, customer_external_id and items")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", "", 0, err
	}
	defer tx.Rollback()

	if err = tx.QueryRowContext(ctx, `SELECT id FROM customers WHERE external_id=$1`, o.CustomerExternalID).Scan(&customerID); err != nil {
		return "", "", 0, fmt.Errorf("unknown customer %q", o.CustomerExternalID)
	}

	err = tx.QueryRowContext(ctx, `
		INSERT INTO orders (external_id, customer_id, ordered_at, total_amount)
		VALUES ($1,$2, COALESCE(NULLIF($3,'')::timestamptz, now()), 0)
		ON CONFLICT (external_id) DO UPDATE
		SET customer_id=EXCLUDED.customer_id, ordered_at=EXCLUDED.ordered_at
		RETURNING id`, o.ExternalID, customerID, o.OrderedAt).Scan(&orderID)
	if err != nil {
		return "", "", 0, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM order_items WHERE order_id=$1`, orderID); err != nil {
		return "", "", 0, err
	}
	for _, it := range o.Items {
		var pid string
		var price float64
		if err = tx.QueryRowContext(ctx, `SELECT id, price::float8 FROM products WHERE external_id=$1`, it.ProductExternalID).Scan(&pid, &price); err != nil {
			return "", "", 0, fmt.Errorf("unknown product %q", it.ProductExternalID)
		}
		if it.UnitPrice > 0 {
			price = it.UnitPrice // snapshot the provided historical price
		}
		q := it.Quantity
		if q <= 0 {
			q = 1
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO order_items (order_id, product_id, quantity, unit_price) VALUES ($1,$2,$3,$4)`, orderID, pid, q, price); err != nil {
			return "", "", 0, err
		}
		total += price * float64(q)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE orders SET total_amount=$1 WHERE id=$2`, total, orderID); err != nil {
		return "", "", 0, err
	}
	return orderID, customerID, total, tx.Commit()
}

// Overview powers the dashboard header.
func (s *Store) Overview(ctx context.Context) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	row := s.DB.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM customers),
		       (SELECT COUNT(*) FROM products),
		       (SELECT COUNT(*) FROM orders),
		       (SELECT COALESCE(SUM(total_amount),0)::float8 FROM orders),
		       (SELECT COUNT(*) FROM campaigns)`)
	var customers, products, orders, campaigns int
	var revenue float64
	if err := row.Scan(&customers, &products, &orders, &revenue, &campaigns); err != nil {
		return nil, err
	}
	out["customers"] = customers
	out["products"] = products
	out["orders"] = orders
	out["revenue"] = revenue
	out["campaigns"] = campaigns
	return out, nil
}
