// Package closing owns the two halves of the closing path that speak SQL: the
// sweep that discovers expired auctions inside the auctiond, and the guarded
// UPDATE that materialises the column inside the closerd.
//
// Neither half knows Redis. The sweep publishes through the Publisher
// interface, which is one method wide, and the materialiser never leaves
// Postgres at all.
package closing

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samuka7abr/bid-storm/internal/stream"
)

const (
	// sweepEvery is the tick. Half a tick of average wait is included in
	// auction_close_lag_seconds on purpose: it is part of what the operator
	// asked (decisão 76).
	sweepEvery = time.Second

	// suppressFor is how long the same id is not republished. Without it a
	// closerd parked for ten minutes would accumulate six hundred copies per
	// expired auction — harmless, since the second application writes nothing,
	// but enough to turn the queue reading into noise (decisão 70).
	suppressFor = 30 * time.Second
)

// expiredSQL is the whole discovery mechanism. It returns zero rows in every
// benchmark cell of this stage, because ENDS_IN is loose enough that nothing
// dies mid-cell — and it is still a query per second the auctiond did not pay
// before, charged equally to the three engines (decisão 69).
//
// No new index backs it: the table holds between one and a thousand rows in
// every cell of this project, and bumping the schema version would turn every
// binary older than this PR red on /readyz (decisão 71).
const expiredSQL = `SELECT id FROM auctions
 WHERE status = 'open' AND ends_at <= now()
 ORDER BY ends_at
 LIMIT 500`

// Publisher is the transport, seen from here: two methods, no Redis.
//
// EnsureGroup is part of the seam and not only a boot step because the group can
// disappear under a running producer: the reset between cells flushes Redis
// (decisão 13), which takes the stream and the group with it. The producer owns
// the group (decisão 72), so the producer is what has to put it back — and the
// sweep is its only periodic caller.
type Publisher interface {
	EnsureGroup(ctx context.Context) error
	Publish(ctx context.Context, e stream.Expired) error
}

// Expirer publishes the fact that an auction has expired, over and over, for as
// long as the fact remains true.
//
// It republishes instead of publishing once, and that is what replaces a
// transactional outbox (decisão 70). An outbox solves "I published and the
// process died before the commit", which cannot happen here: there is no
// transaction producing the fact. The fact is ends_at <= now(), which the
// database recomputes for free on every sweep — so a lost entry, a restarted
// Redis and a recreated group all heal with no recovery code at all.
type Expirer struct {
	pool *pgxpool.Pool
	pub  Publisher
	log  *slog.Logger

	// published is the suppression window, and it is the process's own: a
	// restarted auctiond republishes everything still open and expired. That is
	// the wanted behaviour and not the exception — a restart is exactly when
	// nothing is known about what was published.
	published map[uuid.UUID]time.Time
}

// NewExpirer wires the sweep. It touches nothing until Run or scan is called.
func NewExpirer(pool *pgxpool.Pool, p Publisher, log *slog.Logger) *Expirer {
	return &Expirer{pool: pool, pub: p, log: log, published: make(map[uuid.UUID]time.Time)}
}

// Run ticks until ctx ends. It runs as a goroutine of the auctiond and dies with
// the HTTP server, on the same signal context.
func (e *Expirer) Run(ctx context.Context) {
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.scan(ctx); err != nil && ctx.Err() == nil {
				e.log.Error("sweep for expired auctions", "error", err)
			}
		}
	}
}

// scan is one pass: ask the database, prune the window, publish what is not
// suppressed.
//
// A publication that fails is logged and skipped, never fatal: the id stays out
// of the window, so the next pass tries it again a second later. That is
// precisely what the republishing design bought.
func (e *Expirer) scan(ctx context.Context) error {
	// First, and before any publication: entries added to a stream with no
	// group are invisible to a group created afterwards at $, so publishing
	// into a flushed Redis would drop the fact for a whole suppression window.
	// It costs one round-trip per second, off the hot path, and answers
	// BUSYGROUP immediately in every pass but the first after a wipe.
	if err := e.pub.EnsureGroup(ctx); err != nil {
		return fmt.Errorf("ensure the consumer group: %w", err)
	}

	rows, err := e.pool.Query(ctx, expiredSQL)
	if err != nil {
		return fmt.Errorf("query expired auctions: %w", err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("read expired auction: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read expired auctions: %w", err)
	}

	now := time.Now()
	// Pruned by age on every pass, which is what keeps the map bounded to what
	// expired in the last window instead of to everything ever published.
	for id, at := range e.published {
		if now.Sub(at) >= suppressFor {
			delete(e.published, id)
		}
	}

	for _, id := range ids {
		if _, suppressed := e.published[id]; suppressed {
			continue
		}
		if err := e.pub.Publish(ctx, stream.Expired{AuctionID: id}); err != nil {
			if ctx.Err() == nil {
				e.log.Error("publish expiry", "auction_id", id, "error", err)
			}
			continue
		}
		e.published[id] = now
	}
	return nil
}
