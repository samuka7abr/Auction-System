// Package shard implements BidEngine as a single writer per auction.
//
// Where the optimistic and pessimistic engines make Postgres the point of
// synchronisation — one detects a collision after trying, the other prevents it
// by locking the row — this engine removes the collision instead. Every auction
// routes to exactly one of eight shard goroutines, and that goroutine is the
// only writer of that auction's state. There is no lock to contend for, so
// Postgres stops being the arbiter and becomes durability only.
//
// The package imports pgx, pgxpool and uuid, and nothing else the other two
// engines don't already import — no Prometheus, no internal/metrics, no Gin, no
// internal/app. The metrics this spec is measured by come from the decorator in
// internal/app, wrapped around this engine exactly like the other two (decisão
// 58): the mechanism this package adds — routing, ownership, batching, commit,
// recovery — has no series of its own yet.
package shard

import (
	"context"
	"hash/fnv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samuka7abr/bid-storm/internal/bid"
)

// Fixed by decisão 56: shards, inbox and batch are constants, never
// environment variables. A sweep of any of the three would be etapa 5 measuring
// this engine's own tuning instead of the three strategies against each other.
const numShards = 8

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
func New(pool *pgxpool.Pool) *Engine {
	e := &Engine{}
	for i := range e.shards {
		sh := newShard(pool)
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

	cmd := &command{req: req, resp: make(chan cmdResult, 1)}
	select {
	case sh.inbox <- cmd:
	case <-ctx.Done():
		return bid.BidResult{}, ctx.Err()
	}

	r := <-cmd.resp
	return r.res, r.err
}

// shardFor is deterministic and pure: the same auction always lands on the same
// shard, in the same process, derived only from the bytes of its UUID — no
// state, no I/O, so routing_test.go can check it without a database.
func shardFor(auctionID uuid.UUID, n int) int {
	h := fnv.New32a()
	h.Write(auctionID[:])
	return int(h.Sum32() % uint32(n))
}
