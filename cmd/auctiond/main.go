// Command auctiond serves the auction API.
//
// It runs exactly one bid strategy per process, chosen at boot by BID_STRATEGY:
// the benchmark compares three engines under one configuration, and a process
// that could switch mid-run would be measuring the switch.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/samuka7abr/bid-storm/internal/app"
	"github.com/samuka7abr/bid-storm/internal/closing"
	"github.com/samuka7abr/bid-storm/internal/config"
	"github.com/samuka7abr/bid-storm/internal/db"
	"github.com/samuka7abr/bid-storm/internal/httpapi"
	"github.com/samuka7abr/bid-storm/internal/metrics"
	"github.com/samuka7abr/bid-storm/internal/store"
	"github.com/samuka7abr/bid-storm/internal/stream"
)

const shutdownGrace = 10 * time.Second

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("auctiond stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBPoolSize)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Fails the boot when Redis cannot be reached, for the same reason an
	// unknown strategy does: serving bids with the idempotency middleware
	// answering 503 to every keyed request is a whole cell of nothing. Redis
	// going away later is a different case — the process stays up, /readyz
	// turns red and /healthz stays green.
	rdb, err := db.NewRedis(ctx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer rdb.Close()

	// A schema divergence does not kill the process. It keeps /healthz green and
	// /readyz red, so the cause reaches the orchestrator instead of turning into
	// a mute crashloop.
	schemaErr := db.CheckSchema(ctx, pool)
	found := db.ExpectedSchemaVersion
	var schemaFault *db.SchemaError
	if errors.As(schemaErr, &schemaFault) {
		found = schemaFault.Found
	}

	log.Info("auctiond boot",
		"strategy", cfg.BidStrategy,
		"pool_size", cfg.DBPoolSize,
		"schema_version", found,
		"addr", cfg.HTTPAddr,
	)
	if schemaErr != nil {
		log.Error("schema check failed, /readyz will answer 503",
			"expected", db.ExpectedSchemaVersion,
			"found", found,
			"error", schemaErr.Error(),
		)
	}

	registry := metrics.NewRegistry()
	metrics.RegisterPool(registry, pool)

	// The closing path of etapa 4, and the whole of what this process pays for
	// it: a producer fed by a sweep, never by a bid. No engine publishes
	// anything and no XADD reaches PlaceBid — an XADD in the hot path would make
	// bid_confirm_duration_seconds measure Redis exactly at the ends_at edge,
	// which is the entire scenario of this project (decisão 69).
	producer := stream.NewProducer(rdb, metrics.NewStream(registry))

	// The producer creates the group, at boot, before the first publication: a
	// group created by the consumer would leave everything published before the
	// worker booted invisible to it, and would leave the two gauges below with
	// nothing to read (decisão 72). Failing here aborts the boot for the same
	// reason an unreachable Redis does — an auctiond publishing into the void
	// goes unnoticed until the end of the cell.
	if err := producer.EnsureGroup(ctx); err != nil {
		return err
	}

	// Both queue gauges are published here, by the process that stays alive
	// during the four failure scenarios (decisão 73).
	metrics.RegisterStreamGroup(registry, producer)

	// A strategy with no engine behind it aborts the boot instead of serving a
	// whole benchmark cell's worth of 503s.
	engine, err := app.NewEngine(cfg.BidStrategy, pool, registry)
	if err != nil {
		return err
	}

	// Above the strategy switch, and the only place the strategy name reaches
	// it: the middleware carries that label and never learns which engine
	// answers behind it.
	idempotency := app.NewIdempotency(rdb, registry, cfg.BidStrategy, log)

	gin.SetMode(gin.ReleaseMode)
	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.New(httpapi.Deps{
			// Three conditions, in the order that tells the operator what to
			// do: unreachable Postgres, wrong schema, unreachable Redis. The
			// first that fails is the one /readyz names.
			Ready: []httpapi.Condition{
				{Name: "database", Probe: pool.Ping},
				{Name: "schema", Probe: func(ctx context.Context) error { return db.CheckSchema(ctx, pool) }},
				{Name: "redis", Probe: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
			},
			Metrics:     metrics.Handler(registry),
			Engine:      engine,
			Auctions:    store.New(pool),
			Idempotency: idempotency,
			Log:         log,
		}),
	}

	// One query per second, off the hot path, ended by the same signal context
	// as the server.
	go closing.NewExpirer(pool, producer, log).Run(ctx)

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	log.Info("auctiond draining", "grace", shutdownGrace.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
