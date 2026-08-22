package shard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samuka7abr/bid-storm/internal/bid"
)

// Fixed by decisão 56 alongside numShards: no environment variable makes any
// of these an experimental axis.
const (
	inboxCap  = 1024
	batchMax  = 256
	lingerMax = time.Millisecond // decisão 52: the 2ms ticker of estrategias.md does not exist here.
)

// A shard commit never runs under a caller's context (decisão 54): one batch
// belongs to many callers, and the one who gave up on BID_DEADLINE cannot
// cancel the durability the other 255 were promised. hydrateTimeout exists for
// the same reason applied to the one round-trip hydration pays.
const (
	hydrateTimeout = 5 * time.Second
	commitTimeout  = 5 * time.Second
)

// command is one PlaceBid call in flight. resp has capacity 1 so the shard
// goroutine can always hand back an answer without blocking on a reader
// (RF06).
type command struct {
	req bid.BidRequest
	// startedAt is stamped by PlaceBid before the enqueue: the clock of
	// bid_accept_duration_seconds starts one function call away from where the
	// decorator starts confirm's, which is as close as it gets without this
	// package importing the decorator (decisão 62).
	startedAt time.Time
	resp      chan cmdResult
}

type cmdResult struct {
	res bid.BidResult
	err error
}

func (c *command) reply(res bid.BidResult, err error) {
	c.resp <- cmdResult{res: res, err: err}
}

// pendingBid is a command already decided as Accepted: it consumed a seq and
// moved the top of the in-memory auction, so it is history and stays in the
// batch no matter what happens to its caller's context (decisão 54).
type pendingBid struct {
	cmd       *command
	bidID     uuid.UUID
	auctionID uuid.UUID
	userID    uuid.UUID
	amount    int64
	seq       int64
	key       string
	createdAt time.Time
	// decidedAt is a plain local instant, never createdAt: that one is the
	// decision corrected by the database offset (decisões 50 and 51), and
	// time.Since over it would return the commit plus the skew between two
	// clocks — plausible, silent, and wrong by exactly that much (decisão 62).
	decidedAt time.Time
	current   bid.AuctionState // snapshot for the eventual 201's Current
}

// shard is one goroutine's exclusive ownership of a slice of auctions: inbox
// and auctions are read and written only from run, with no mutex — that is the
// thing this engine has to prove (RF01).
type shard struct {
	pool     *pgxpool.Pool
	inbox    chan *command
	auctions map[uuid.UUID]*auctionState
	obs      Observers
}

func newShard(pool *pgxpool.Pool, obs Observers) *shard {
	return &shard{
		pool:     pool,
		inbox:    make(chan *command, inboxCap),
		auctions: make(map[uuid.UUID]*auctionState),
		obs:      obs,
	}
}

// run is the only hand-written concurrency in the project: receive, decide,
// accumulate, commit, evict on failure — one goroutine, one inbox, one map.
func (s *shard) run() {
	for cmd := range s.inbox {
		first := s.decide(cmd)
		if first == nil {
			// Rejected, not found, closed, or an infrastructure error: cmd
			// already has its answer. Nothing entered a batch, so there is
			// nothing to commit — go back to waiting on the inbox.
			continue
		}

		batch := []*pendingBid{first}
		opened := time.Now()

	accumulate:
		for len(batch) < batchMax {
			select {
			case next := <-s.inbox:
				if p := s.decide(next); p != nil {
					batch = append(batch, p)
				}
			default:
				break accumulate // inbox empty: commit now, latency matches the other two engines
			}
			if time.Since(opened) > lingerMax {
				break accumulate // linger: the accept cannot wait behind an unbroken stream of rejections
			}
		}

		s.commit(batch)
	}
}

// decide classifies one command against this shard's state, hydrating on a
// miss. It answers rejections and infrastructure failures directly and returns
// nil; an accept updates the in-memory state and returns the pendingBid for the
// caller to fold into the open batch.
func (s *shard) decide(cmd *command) *pendingBid {
	req := cmd.req

	st, ok := s.auctions[req.AuctionID]
	if !ok {
		ctx, cancel := context.WithTimeout(context.Background(), hydrateTimeout)
		hydrated, err := hydrate(ctx, s.pool, req.AuctionID)
		cancel()

		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Absence is never cached (decisão 49): an auction POST /auctions
			// creates a moment later must still be found.
			cmd.reply(bid.BidResult{Outcome: bid.NotFound}, nil)
			return nil
		case err != nil:
			cmd.reply(bid.BidResult{}, fmt.Errorf("hydrate auction %s: %w", req.AuctionID, err))
			return nil
		}
		st = hydrated
		s.auctions[req.AuctionID] = st
	}

	switch {
	case st.IsClosed(st.now()):
		cmd.reply(bid.BidResult{Outcome: bid.Closed, Current: st.AuctionState}, nil)
		return nil
	case req.AmountCents < st.MinNextBid():
		// The freshest state that exists, not the last one committed: this
		// engine's rejections carry an advantage the optimistic one's do not
		// (decisão 57).
		cmd.reply(bid.BidResult{Outcome: bid.TooLow, Current: st.AuctionState}, nil)
		return nil
	}

	st.Version++
	st.HighestBidCents = req.AmountCents
	st.highestBidder = req.UserID

	// Only the accept is observed. A rejection has no series of its own
	// (decisão 66), and neither NotFound nor an infrastructure failure is a
	// decision this histogram is asked about.
	decidedAt := time.Now()
	s.obs.Accept.Observe(decidedAt.Sub(cmd.startedAt).Seconds())

	return &pendingBid{
		cmd:       cmd,
		bidID:     uuid.New(),
		auctionID: req.AuctionID,
		userID:    req.UserID,
		amount:    req.AmountCents,
		seq:       st.Version,
		key:       req.IdempotencyKey,
		createdAt: st.now(),
		decidedAt: decidedAt,
		current:   st.AuctionState,
	}
}

// commit runs the batch's single statement and only then answers every command
// in it — Accepted on success, an error on failure, never both for the same
// batch (decisão 8: 201 means durable).
func (s *shard) commit(batch []*pendingBid) {
	ids := make([]uuid.UUID, len(batch))
	auctions := make([]uuid.UUID, len(batch))
	users := make([]uuid.UUID, len(batch))
	amounts := make([]int64, len(batch))
	seqs := make([]int64, len(batch))
	keys := make([]string, len(batch))
	createdAts := make([]time.Time, len(batch))

	// One UPDATE row per distinct auction the batch touched. This never needs
	// to deduplicate: within a shard, a second accept for the same auction can
	// only exist after the first one's 201 reached its caller, which cannot
	// happen before this very commit returns (decisão 48).
	upAuctions := make([]uuid.UUID, len(batch))
	upAmounts := make([]int64, len(batch))
	upBidders := make([]uuid.UUID, len(batch))
	upVersions := make([]int64, len(batch))

	for i, p := range batch {
		ids[i], auctions[i], users[i] = p.bidID, p.auctionID, p.userID
		amounts[i], seqs[i], keys[i], createdAts[i] = p.amount, p.seq, p.key, p.createdAt
		upAuctions[i], upAmounts[i], upBidders[i], upVersions[i] = p.auctionID, p.amount, p.userID, p.seq
	}

	// Observed before the statement and regardless of its outcome: the size is a
	// property of the accumulation loop, which has already finished. Making it
	// conditional on success would silence the series exactly in the incident
	// someone would use it to explain — the batch that aborted (decisão 65).
	s.obs.Batch.Observe(float64(len(batch)))

	ctx, cancel := context.WithTimeout(context.Background(), commitTimeout)
	defer cancel()

	_, err := s.pool.Exec(ctx, commitBatch,
		ids, auctions, users, amounts, seqs, keys, createdAts,
		upAuctions, upAmounts, upBidders, upVersions,
	)
	if err != nil {
		slog.Error("shard batch commit failed", "batch_size", len(batch), "error", err)
		for _, p := range batch {
			// The memory must never sit ahead of the database (decisão 53): the
			// next command for any of these auctions re-hydrates the truth.
			delete(s.auctions, p.auctionID)
			p.cmd.reply(bid.BidResult{}, fmt.Errorf("commit batch: %w", err))
		}
		return
	}

	for _, p := range batch {
		// The lag, unlike the batch size, only exists here: a batch that aborted
		// has no instant of durability to measure to (decisão 65).
		s.obs.Lag.Observe(time.Since(p.decidedAt).Seconds())
		p.cmd.reply(bid.BidResult{
			Outcome: bid.Accepted,
			Seq:     p.seq,
			BidID:   p.bidID,
			Current: p.current,
		}, nil)
	}
}
