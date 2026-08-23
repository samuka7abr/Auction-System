// Command closerd materialises the closing of expired auctions.
//
// It responds for performance, never for correctness (decisão 12): killing it
// mid-message cannot let a late bid in, cannot close an auction early and
// cannot close the same auction twice — the guards in the UPDATE make all three
// impossible. The worst it can do is delay the column.
//
// It is a second process, on a second port, with a registry of its own, and it
// imports neither internal/httpapi nor Gin (decisão 74): the router is the bid
// contract — envelope, identity, idempotency middleware — and none of that
// exists here. Three routes on a http.ServeMux are less code than the adapter
// would be.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/samuka7abr/bid-storm/internal/closing"
	"github.com/samuka7abr/bid-storm/internal/config"
	"github.com/samuka7abr/bid-storm/internal/db"
	"github.com/samuka7abr/bid-storm/internal/metrics"
	"github.com/samuka7abr/bid-storm/internal/stream"
)

const (
	// addr is fixed, like every other knob of this process: the port mapped by
	// the compose is CLOSERD_PORT, and no binary reads it (decisão 56).
	addr = ":8081"

	// poolSize is four because the loop is sequential and uses one connection at
	// a time (decisão 77); the other three are slack for the probes.
	poolSize = 4

	shutdownGrace = 10 * time.Second

	// probeTimeout caps /readyz so a hung dependency answers 503 instead of
	// hanging the scrape.
	probeTimeout = 2 * time.Second
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("closerd stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	// The same config package as the auctiond, and BID_STRATEGY is ignored here:
	// this process has no engine and no opinion about which one is running.
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL, poolSize)
	if err != nil {
		return err
	}
	defer pool.Close()

	rdb, err := db.NewRedis(ctx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer rdb.Close()

	// Registered before the first message, so the four series exist at zero on a
	// worker that has closed nothing.
	registry := metrics.NewRegistry()
	materializer := closing.NewMaterializer(pool, metrics.NewMaterializer(registry))
	consumer := stream.NewConsumer(rdb, stream.Name(), metrics.NewStream(registry), log)

	log.Info("closerd boot", "addr", addr, "pool_size", poolSize, "consumer", stream.Name())

	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler(registry))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Liveness never touches a dependency: a Redis that went away must not
		// be mistaken for a dead process and restarted.
		write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Two conditions and not three: the worker does not check the schema
	// version. It has no SQL that depends on a new column, and a third copy of a
	// rule that already lives in two places is a rule that drifts.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		probes := []struct {
			name  string
			probe func(context.Context) error
		}{
			{"database", pool.Ping},
			{"redis", func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
		}
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		for _, p := range probes {
			if err := p.probe(ctx); err != nil {
				write(w, http.StatusServiceUnavailable, map[string]string{
					"status": "unready", "check": p.name, "error": err.Error(),
				})
				return
			}
		}
		write(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	runErr := make(chan error, 1)
	go func() {
		runErr <- consumer.Run(ctx, func(ctx context.Context, e stream.Expired) error {
			_, err := materializer.Close(ctx, e.AuctionID)
			return err
		})
	}()

	select {
	case err := <-serveErr:
		return err
	case err := <-runErr:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		// The loop finishes the message in its hand before it returns.
		if err := <-runErr; err != nil {
			return err
		}
	}

	log.Info("closerd draining", "grace", shutdownGrace.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func write(w http.ResponseWriter, status int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
