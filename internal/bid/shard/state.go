package shard

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samuka7abr/bid-storm/internal/bid"
)

// auctionState is everything one shard goroutine knows about one auction: the
// contract's AuctionState — the exact type BidResult.Current publishes — plus
// the two fields the contract has no room for. highestBidder is the winner the
// decision wrote (RF03's "grava topo e vencedor em memória"); skew is the
// database clock offset measured at hydration.
type auctionState struct {
	bid.AuctionState
	highestBidder uuid.UUID
	skew          time.Duration
}

// now stands in for the database's clock without asking it again: time.Now
// corrected by the offset hydration measured (decisão 50). Every closing
// decision uses this and never time.Now directly, so the guard agrees with the
// UPDATE the batch will eventually run under the same authority (decisão 22).
func (s *auctionState) now() time.Time {
	return time.Now().Add(s.skew)
}

// hydrate reads an auction this shard has never seen, on the caller's own
// goroutine — never handed off, so the total order per auction stays a
// consequence of the channel instead of depending on code (decisão 49).
//
// A miss (pgx.ErrNoRows) and any infrastructure error come back untouched: the
// caller decides what each means, and neither is written to the map.
func hydrate(ctx context.Context, pool *pgxpool.Pool, auctionID uuid.UUID) (*auctionState, error) {
	var (
		status string
		st     bid.AuctionState
		dbNow  time.Time
	)

	// localMid is the average of the instants surrounding the round-trip, so the
	// offset's error is at most half the round-trip rather than the whole of it
	// (decisão 50).
	before := time.Now()
	err := pool.QueryRow(ctx, hydrateQuery, auctionID).Scan(
		&st.Version, &st.HighestBidCents, &st.MinIncrementCents, &status, &st.EndsAt, &dbNow,
	)
	after := time.Now()
	if err != nil {
		return nil, err
	}
	st.Status = bid.Status(status)

	localMid := before.Add(after.Sub(before) / 2)
	return &auctionState{AuctionState: st, skew: dbNow.Sub(localMid)}, nil
}
