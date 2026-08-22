// Package app is the only place in the code that knows the strategy names.
//
// The contract in internal/bid cannot import the engines: the conformance suite
// imports the contract and every engine imports it too, so whatever knows all
// three would close an import cycle if it lived there. Choosing here makes that
// cycle impossible and keeps internal/bid free of what the single-writer engine
// brings in etapa 3.
package app

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/samuka7abr/bid-storm/internal/bid"
	"github.com/samuka7abr/bid-storm/internal/bid/optimistic"
	"github.com/samuka7abr/bid-storm/internal/bid/pessimistic"
	"github.com/samuka7abr/bid-storm/internal/bid/shard"
	"github.com/samuka7abr/bid-storm/internal/metrics"
)

// The three values BID_STRATEGY accepts across the project's lifetime. One of
// them is not implemented yet, and it is named here anyway so the failure says
// which etapa it arrives in.
const (
	StrategyOptimistic  = "optimistic"
	StrategyPessimistic = "pessimistic"
	StrategyShard       = "shard"
)

// NewEngine returns the engine BID_STRATEGY selects, already wrapped in the
// metrics decorator — the wrapping happens here so that no engine can be built
// unmeasured, and so all three are timed at one boundary — and in the carrier
// that hands it the idempotency key of the request.
//
// Unimplemented and unknown strategies fail at boot rather than at the first
// bid: a process that comes up and only then discovers it has no engine turns a
// typo into a benchmark cell full of 503s.
func NewEngine(strategy string, pool *pgxpool.Pool, reg prometheus.Registerer) (bid.BidEngine, error) {
	var engine bid.BidEngine
	switch strategy {
	case StrategyOptimistic:
		engine = optimistic.New(pool)
	case StrategyPessimistic:
		// The lock observer is built here, off the same registry, because
		// internal/metrics owns every series name and the engine is handed a
		// one-method interface instead. Wrapping still happens outside it: what
		// compares the three strategies is measured at one boundary, and only
		// what describes this mechanism is measured from within (decisão 28).
		engine = pessimistic.New(pool, metrics.NewLockWait(reg))
	case StrategyShard:
		// Two calls and not one because the order forces it: the observers exist
		// before the engine, and the engine exists before the collector that
		// samples its inboxes at scrape time. Both happen against the registry
		// and neither touches the pool, so booting still fails on the right line
		// when Postgres is slow to come up.
		accept, lag, batch := metrics.NewShard(reg)
		sh := shard.New(pool, shard.Observers{Accept: accept, Lag: lag, Batch: batch})
		metrics.RegisterShardInbox(reg, sh)
		engine = sh
	default:
		return nil, fmt.Errorf("BID_STRATEGY=%q is not a strategy: want %s, %s or %s",
			strategy, StrategyOptimistic, StrategyPessimistic, StrategyShard)
	}
	return carrier{next: metrics.Instrument(engine, reg, strategy)}, nil
}
