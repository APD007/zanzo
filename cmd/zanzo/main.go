// Command zanzo runs the authorization service.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/APD007/zanzo/internal/api"
	"github.com/APD007/zanzo/internal/check"
	"github.com/APD007/zanzo/internal/schema"
	"github.com/APD007/zanzo/internal/storage"
)

func main() {
	var (
		addr       = flag.String("addr", ":8080", "listen address")
		dsn        = flag.String("dsn", os.Getenv("ZANZO_POSTGRES"), "Postgres DSN; in-memory store when empty")
		schemaPath = flag.String("schema", "", "path to a schema file (required)")
		noCache    = flag.Bool("no-cache", false, "disable the check cache (for benchmarking a cold path)")
	)
	flag.Parse()

	if *schemaPath == "" {
		log.Fatal("-schema is required")
	}
	src, err := os.ReadFile(*schemaPath)
	if err != nil {
		log.Fatalf("read schema: %v", err)
	}
	sch, err := schema.Parse(string(src))
	if err != nil {
		// A schema that does not load must stop the process. Starting with a
		// partial schema would deny requests that ought to succeed, which is
		// an outage that looks like a permissions bug.
		log.Fatalf("parse schema: %v", err)
	}

	var store storage.Store
	if *dsn == "" {
		log.Print("no DSN given; using the in-memory store")
		store = storage.NewMemory()
	} else {
		db, err := sql.Open("postgres", *dsn)
		if err != nil {
			log.Fatalf("open postgres: %v", err)
		}
		// The engine fans out concurrent subproblems, so the pool is the real
		// concurrency limit for a check. Too small and checks queue on
		// connections rather than on work.
		db.SetMaxOpenConns(32)
		db.SetMaxIdleConns(32)
		db.SetConnMaxLifetime(30 * time.Minute)
		pg := storage.NewPostgres(db)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := pg.Migrate(ctx); err != nil {
			cancel()
			log.Fatalf("migrate: %v", err)
		}
		cancel()
		defer db.Close()
		store = pg
	}

	engine := check.New(sch, store)
	if !*noCache {
		engine.Cache = check.NewRevisionKeyed()
	}

	stats := newStats()
	srv := &api.Server{
		Engine:  engine,
		Store:   store,
		Observe: stats.observe,
		Logger:  log.New(os.Stderr, "zanzo ", log.LstdFlags|log.Lmsgprefix),
	}

	mux := srv.Routes()
	mux.HandleFunc("GET /stats", stats.handler)

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("zanzo listening on %s (cache=%v, store=%s)",
			*addr, !*noCache, storeName(*dsn))
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	// Drain in-flight requests rather than cutting them off: an authorization
	// check that dies mid-flight looks like a denial to the caller.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Print("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func storeName(dsn string) string {
	if dsn == "" {
		return "memory"
	}
	return "postgres"
}

// stats keeps a latency histogram per route. Deliberately tiny: the point is
// to report p50/p95/p99/max from the server's own view, so a load test's
// numbers can be checked against something that never crossed the network.
type stats struct {
	mu       sync.Mutex
	samples  map[string][]time.Duration
	statuses map[string]map[int]int
}

func newStats() *stats {
	return &stats{
		samples:  map[string][]time.Duration{},
		statuses: map[string]map[int]int{},
	}
}

func (s *stats) observe(route string, status int, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples[route] = append(s.samples[route], d)
	if s.statuses[route] == nil {
		s.statuses[route] = map[int]int{}
	}
	s.statuses[route][status]++
}

func (s *stats) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for route, xs := range s.samples {
		sorted := append([]time.Duration(nil), xs...)
		sortDurations(sorted)
		fmt.Fprintf(w, "%s n=%d p50=%v p95=%v p99=%v max=%v statuses=%v\n",
			route, len(sorted),
			pct(sorted, 0.50), pct(sorted, 0.95), pct(sorted, 0.99),
			sorted[len(sorted)-1], s.statuses[route])
	}
}

func pct(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)-1))
	return sorted[i]
}

func sortDurations(xs []time.Duration) {
	// Insertion sort is fine: this runs once per /stats request, not per check.
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
