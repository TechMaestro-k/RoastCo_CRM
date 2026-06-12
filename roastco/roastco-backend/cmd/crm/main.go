// cmd/crm is the CRM backend: HTTP API + dispatch worker pool in one binary.
// (One process keeps the free-tier deploy simple; the dispatcher is its own
// package and could run as a separate deployment without code changes.)
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/roastco/backend/internal/ai"
	"github.com/roastco/backend/internal/api"
	"github.com/roastco/backend/internal/attribution"
	"github.com/roastco/backend/internal/dispatch"
	"github.com/roastco/backend/internal/envfile"
	"github.com/roastco/backend/internal/store"
	"github.com/roastco/backend/migrations"
)

func main() {
	envfile.Load()
	ctx := context.Background()

	s, err := store.Open()
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	if err := s.Migrate(ctx, migrations.Schema); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Println("migrations applied")

	planner := ai.New()
	log.Printf("ai mode: %s", planner.Mode())

	attr := attribution.New(s)
	srv := api.New(s, planner, attr)

	d := dispatch.New(s)
	d.Run(ctx)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("crm listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, srv.Router()))
}
