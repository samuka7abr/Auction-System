// Package shard implements BidEngine as a single writer per auction.
//
// Where the optimistic and pessimistic engines make Postgres the point of
// synchronisation — one detects a collision after trying, the other prevents it
// by locking the row — this engine removes the collision instead. Every auction
// routes to exactly one of eight shard goroutines, and that goroutine is the
// only writer of that auction's state. There is no lock to contend for, so
// Postgres stops being the arbiter and becomes durability only.
//
// The package imports pgx, pgxpool, uuid and — since spec 02 — Prometheus, for
// the Observer type alone, exactly as internal/bid/pessimistic already does
// (decisão 67). It still does not import internal/metrics, internal/app or Gin:
// the three series that describe this mechanism are named in internal/metrics
// and reach the engine as three one-method interfaces, so no series name lives
// here. What compares the three strategies keeps coming from the decorator in
// internal/app, wrapped around this engine exactly like the other two.
package shard

import (
	"context"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/samuka7abr/bid-storm/internal/bid"
)

// Fixed by decisão 56: shards, inbox and batch are constants, never
// environment variables. A sweep of any of the three would be etapa 5 measuring
// this engine's own tuning instead of the three strategies against each other.
const numShards = 8

// Observers is what internal/metrics hands this engine: three series it feeds
// without ever learning their names. The zero value is a working engine that
// publishes nothing, which is what the conformance suite and the boot test
// build.
type Observers struct {
	Accept prometheus.Observer // bid_accept_duration_seconds
	Lag    prometheus.Observer // journal_lag_seconds
	Batch  prometheus.Observer // shard_batch_size
}

func (o Observers) orDiscard() Observers {
	discard := prometheus.ObserverFunc(func(float64) {})
	if o.Accept == nil {
		o.Accept = discard
	}
	if o.Lag == nil {
		o.Lag = discard
	}
	if o.Batch == nil {
		o.Batch = discard
	}
	return o
}

// Engine owns numShards goroutines, each with exclusive access to a slice of
// auctions. New starts them; they run for the life of the process.
type Engine struct {
	shards [numShards]*shard
}

// New returns an engine whose shards are already running, and never touches
// pool itself: dialling happens lazily, inside a shard, on the first bid for an
// auction it has not seen. That is what lets internal/app build this engine with
// pool == nil in boot tests, without the failure landing on the wrong line
// (decisão 49, RF02).
func New(pool *pgxpool.Pool, obs Observers) *Engine {
	obs = obs.orDiscard()
	e := &Engine{}
	for i := range e.shards {
		sh := newShard(pool, obs)
		e.shards[i] = sh
		go sh.run()
	}
	return e
}

// PlaceBid hands req to the auction's shard and waits for its answer.
//
// Enqueueing races the inbox against ctx: cancelled before it is accepted, the
// caller gets an error and nothing was decided. Accepted, the command is
// history (decisão 54) — PlaceBid then waits for the response with no early
// exit, because the shard already promised a 201 to whoever else is in the same
// batch and one caller giving up cannot cancel the durability of the rest.
func (e *Engine) PlaceBid(ctx context.Context, req bid.BidRequest) (bid.BidResult, error) {
	sh := e.shards[shardFor(req.AuctionID, numShards)]

	// Stamped before the enqueue, not after: the wait to enter the inbox is cost
	// of the mechanism, and starting the clock inside the shard goroutine would
	// measure the stretch nobody doubts is fast while hiding the one where the
	// engine can choke (decisão 62).
	cmd := &command{req: req, startedAt: time.Now(), resp: make(chan cmdResult, 1)}
	select {
	case sh.inbox <- cmd:
	case <-ctx.Done():
		return bid.BidResult{}, ctx.Err()
	}

	r := <-cmd.resp
	return r.res, r.err
}

// InboxDepths reports len(inbox) for every shard, in shard order, and is safe
// to call from any goroutine: len over a channel is a read, and it is the whole
// cost shard_inbox_depth charges — paid by the scrape, never by a bid.
func (e *Engine) InboxDepths() []int {
	depths := make([]int, len(e.shards))
	for i, sh := range e.shards {
		depths[i] = len(sh.inbox)
	}
	return depths
}

// shardFor is deterministic and pure: the same auction always lands on the same
// shard, in the same process, derived only from the bytes of its UUID — no
// state, no I/O, so routing_test.go can check it without a database.
func shardFor(auctionID uuid.UUID, n int) int {
	h := fnv.New32a()
	h.Write(auctionID[:])
	return int(h.Sum32() % uint32(n))
}
