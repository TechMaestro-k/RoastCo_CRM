// cmd/seed generates Roast & Co's world (42 products, 800 shoppers, ~2.5k orders) —
// and feeds it through the REAL ingest API (same path as production data, so
// seeding also exercises the ingestion contract and its idempotency).
// Deterministic (fixed RNG seed): run it twice and the second run upserts
// the same rows — counts must not change.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/roastco/backend/internal/envfile"
)

type product struct {
	ExternalID string  `json:"external_id"`
	Name       string  `json:"name"`
	Category   string  `json:"category"`
	Price      float64 `json:"price"`
}

type customer struct {
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	Phone      string `json:"phone"`
	City       string `json:"city"`
	CreatedAt  string `json:"created_at"`
}

type orderItem struct {
	ProductExternalID string `json:"product_external_id"`
	Quantity          int    `json:"quantity"`
}

type order struct {
	ExternalID         string      `json:"external_id"`
	CustomerExternalID string      `json:"customer_external_id"`
	OrderedAt          string      `json:"ordered_at"`
	Items              []orderItem `json:"items"`
}

var catalog = []product{
	// beans
	{"prod-yirgacheffe", "Ethiopian Yirgacheffe 250g", "beans", 649},
	{"prod-huila", "Colombia Huila 250g", "beans", 599},
	{"prod-malabar", "Monsooned Malabar AA 250g", "beans", 549},
	{"prod-attikan", "Attikan Estate 250g", "beans", 525},
	{"prod-kenya-aa", "Kenya AA Nyeri 250g", "beans", 699},
	{"prod-mandheling", "Sumatra Mandheling 250g", "beans", 679},
	{"prod-house-blend", "Roast & Co House Blend 500g", "beans", 899},
	{"prod-midnight", "Midnight Dark Roast 250g", "beans", 575},
	{"prod-chikmagalur", "Single Estate Chikmagalur 250g", "beans", 615},
	{"prod-antigua", "Guatemala Antigua 250g", "beans", 659},
	{"prod-vienna", "Vienna Roast Espresso Blend 250g", "beans", 595},
	{"prod-microlot", "Seasonal Microlot 200g", "beans", 825},
	// ground
	{"prod-house-ground", "House Blend Ground 250g", "ground", 579},
	{"prod-kaapi", "Filter Kaapi Ground 500g", "ground", 499},
	{"prod-fp-colombia", "French Press Grind Colombia 250g", "ground", 609},
	{"prod-moka-dark", "Moka Pot Grind Dark 250g", "ground", 585},
	{"prod-coldbrew-grind", "Cold Brew Coarse Grind 400g", "ground", 699},
	{"prod-espresso-fine", "Espresso Fine Grind 250g", "ground", 605},
	{"prod-pourover-yirg", "Pour-Over Grind Yirgacheffe 250g", "ground", 665},
	{"prod-decaf", "Decaf Swiss Water Ground 250g", "ground", 689},
	// equipment
	{"prod-timemore-c2", "Timemore C2 Hand Grinder", "equipment", 2899},
	{"prod-encore", "Baratza Encore Grinder", "equipment", 13500},
	{"prod-v60-kit", "Hario V60 Dripper Kit", "equipment", 1450},
	{"prod-aeropress", "AeroPress Original", "equipment", 3200},
	{"prod-chemex", "Chemex 6-Cup Brewer", "equipment", 4500},
	{"prod-frenchpress", "French Press 600ml", "equipment", 1650},
	{"prod-kettle", "Electric Gooseneck Kettle", "equipment", 4200},
	{"prod-moka-3", "Moka Pot 3-Cup", "equipment", 1899},
	{"prod-coldbrew-tower", "Cold Brew Tower", "equipment", 6800},
	{"prod-compatto", "Espresso Machine Compatto", "equipment", 28500},
	{"prod-lever", "Manual Lever Espresso Maker", "equipment", 9200},
	{"prod-burr-pro", "Burr Grinder Pro Electric", "equipment", 8900},
	// accessories
	{"prod-pourover-mug", "Ceramic Pour-Over Mug", "accessories", 549},
	{"prod-glass-cups", "Double-Wall Glass Cups (Set of 2)", "accessories", 799},
	{"prod-v60-filters", "V60 Paper Filters (100)", "accessories", 349},
	{"prod-canister", "Airtight Storage Canister", "accessories", 699},
	{"prod-scale", "Coffee Scale with Timer", "accessories", 2750},
	{"prod-tamper", "Tamper 51mm", "accessories", 899},
	{"prod-tumbler", "Travel Tumbler 350ml", "accessories", 949},
	{"prod-cupping-spoon", "Cupping Spoon", "accessories", 399},
	{"prod-knockbox", "Knock Box", "accessories", 1199},
	{"prod-tote", "Roast & Co Tote Bag", "accessories", 449},
}

var firstNames = []string{"Aarav", "Vihaan", "Aditya", "Arjun", "Reyansh", "Kabir", "Ishaan", "Rohan", "Dev", "Kartik",
	"Ananya", "Diya", "Saanvi", "Aadhya", "Myra", "Anika", "Navya", "Kiara", "Riya", "Ira",
	"Shiv", "Vansh", "Harish", "Priya", "Meera", "Tara", "Nikhil", "Rahul", "Sneha", "Pooja"}

var lastNames = []string{"Sharma", "Verma", "Gupta", "Mehta", "Iyer", "Reddy", "Nair", "Kapoor", "Malhotra", "Joshi",
	"Bose", "Chatterjee", "Desai", "Patel", "Singh", "Khanna", "Rao", "Menon", "Agarwal", "Bhatia",
	"Kulkarni", "Pillai", "Saxena", "Trivedi", "Chopra"}

var cities = []string{"Gurgaon", "Mumbai", "Bangalore", "Delhi", "Pune", "Hyderabad", "Chennai", "Jaipur", "Kolkata", "Noida"}

var categories = []string{"beans", "ground", "equipment", "accessories"}

func main() {
	envfile.Load()
	apiURL := os.Getenv("SEED_API_URL")
	if apiURL == "" {
		apiURL = "http://127.0.0.1:8080"
	}
	rng := rand.New(rand.NewSource(42)) // deterministic world
	now := time.Now().UTC()

	// ---- products ----
	post(apiURL+"/api/ingest/products", catalog)
	log.Printf("seeded %d products", len(catalog))

	byCategory := map[string][]product{}
	for _, p := range catalog {
		byCategory[p.Category] = append(byCategory[p.Category], p)
	}

	// ---- customers + orders ----
	nCustomers := 800
	var customers []customer
	var orders []order
	personaCount := map[string]int{}

	for i := 0; i < nCustomers; i++ {
		fn := firstNames[rng.Intn(len(firstNames))]
		ln := lastNames[rng.Intn(len(lastNames))]
		city := cities[rng.Intn(len(cities))]
		ext := fmt.Sprintf("cust-%04d", i)
		email := fmt.Sprintf("%s.%s%d@example.com", strings.ToLower(fn), strings.ToLower(ln), i)
		phone := fmt.Sprintf("+91-9%09d", 100000000+rng.Intn(899999999))
		favCat := categories[rng.Intn(len(categories))]

		// Personas: whales 15%, regulars 35%, one-timers 30%, lapsed 20%.
		var nOrders int
		var lapsed bool
		switch r := rng.Float64(); {
		case r < 0.15:
			nOrders = 6 + rng.Intn(7) // 6–12
			personaCount["whale"]++
		case r < 0.50:
			nOrders = 2 + rng.Intn(4) // 2–5
			personaCount["regular"]++
		case r < 0.80:
			nOrders = 1
			personaCount["one-timer"]++
		default:
			nOrders = 1 + rng.Intn(3) // 1–3, all old
			lapsed = true
			personaCount["lapsed"]++
		}

		oldestDays := 0
		for j := 0; j < nOrders; j++ {
			var daysAgo int
			if lapsed {
				daysAgo = 90 + rng.Intn(311) // 90–400 days ago
			} else if j == 0 && nOrders > 1 {
				daysAgo = rng.Intn(45) // actives have a recent order
			} else {
				daysAgo = rng.Intn(540) // spread over ~18 months
			}
			if daysAgo > oldestDays {
				oldestDays = daysAgo
			}
			orderedAt := now.Add(-time.Duration(daysAgo)*24*time.Hour - time.Duration(rng.Intn(86400))*time.Second)

			nItems := 1 + rng.Intn(3)
			seen := map[string]bool{}
			var items []orderItem
			for k := 0; k < nItems; k++ {
				cat := favCat
				if rng.Float64() > 0.70 {
					cat = categories[rng.Intn(len(categories))]
				}
				p := byCategory[cat][rng.Intn(len(byCategory[cat]))]
				if seen[p.ExternalID] {
					continue
				}
				seen[p.ExternalID] = true
				qty := 1
				if rng.Float64() < 0.2 {
					qty = 2
				}
				items = append(items, orderItem{ProductExternalID: p.ExternalID, Quantity: qty})
			}
			orders = append(orders, order{
				ExternalID:         fmt.Sprintf("ord-%04d-%02d", i, j),
				CustomerExternalID: ext,
				OrderedAt:          orderedAt.Format(time.RFC3339),
				Items:              items,
			})
		}

		signup := now.Add(-time.Duration(oldestDays+10+rng.Intn(110)) * 24 * time.Hour)
		customers = append(customers, customer{
			ExternalID: ext, Name: fn + " " + ln, Email: email, Phone: phone, City: city,
			CreatedAt: signup.Format(time.RFC3339),
		})
	}

	for i := 0; i < len(customers); i += 250 {
		post(apiURL+"/api/ingest/customers", customers[i:min(i+250, len(customers))])
	}
	log.Printf("seeded %d customers (%v)", len(customers), personaCount)

	// Orders: skip attribution (no campaigns exist during seeding) and post
	// batches concurrently. Against a remote database the round-trip latency,
	// not CPU, is the bottleneck — parallel batches hide most of it.
	const batchSize, workers = 200, 6
	var batches [][]order
	for i := 0; i < len(orders); i += batchSize {
		batches = append(batches, orders[i:min(i+batchSize, len(orders))])
	}
	ordersURL := apiURL + "/api/ingest/orders?attribute=false"
	jobs := make(chan []order)
	var wg sync.WaitGroup
	var done int32
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				post(ordersURL, b)
				n := atomic.AddInt32(&done, int32(len(b)))
				log.Printf("orders: %d/%d", n, len(orders))
			}
		}()
	}
	for _, b := range batches {
		jobs <- b
	}
	close(jobs)
	wg.Wait()
	log.Printf("seeded %d orders — done", len(orders))
}

func post(url string, payload interface{}) {
	body, _ := json.Marshal(payload)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("seed: POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&e)
		log.Fatalf("seed: POST %s → %d: %s", url, resp.StatusCode, e["error"])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
