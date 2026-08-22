package shard

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/samuka7abr/bid-storm/internal/bid"
	"github.com/samuka7abr/bid-storm/internal/testsupport"
)

// recorder is a prometheus.Observer with no Prometheus in it: what this file
// checks is who observes what and how many times, and a real histogram would
// only make that harder to read.
type recorder struct {
	mu     sync.Mutex
	values []float64
}

func (r *recorder) Observe(v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = append(r.values, v)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.values)
}

func (r *recorder) sum() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var total float64
	for _, v := range r.values {
		total += v
	}
	return total
}

type recorders struct{ accept, lag, batch *recorder }

func newRecorders() recorders {
	return recorders{accept: &recorder{}, lag: &recorder{}, batch: &recorder{}}
}

func (r recorders) observers() Observers {
	return Observers{Accept: r.accept, Lag: r.lag, Batch: r.batch}
}

// bid_accept_duration_seconds is the cost of deciding, and only an accept is a
// decision this series is asked about: a rejection has no series of its own
// (decisão 66), and NotFound is not a decision at all.
func TestOnlyAcceptsAreObservedInAccept(t *testing.T) {
	pg := testsupport.Start(t)
	rec := newRecorders()
	engine := New(pg.Pool, rec.observers())

	open := seedAuction(t, pg.Pool, uuid.New(), time.Minute)
	closed := seedAuction(t, pg.Pool, uuid.New(), -time.Minute)

	res, err := engine.PlaceBid(context.Background(), bid.BidRequest{
		AuctionID: open, UserID: uuid.New(), AmountCents: 10 * minIncrement,
	})
	if err != nil || res.Outcome != bid.Accepted {
		t.Fatalf("accept: outcome %v, err %v", res.Outcome, err)
	}
	if got := rec.accept.count(); got != 1 {
		t.Fatalf("accept observations after one accepted bid = %d, want 1", got)
	}

	for _, tc := range []struct {
		name string
		req  bid.BidRequest
		want bid.Outcome
	}{
		{"too low", bid.BidRequest{AuctionID: open, UserID: uuid.New(), AmountCents: minIncrement}, bid.TooLow},
		{"not found", bid.BidRequest{AuctionID: uuid.New(), UserID: uuid.New(), AmountCents: minIncrement}, bid.NotFound},
		{"closed", bid.BidRequest{AuctionID: closed, UserID: uuid.New(), AmountCents: 99 * minIncrement}, bid.Closed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := engine.PlaceBid(context.Background(), tc.req)
			if err != nil || res.Outcome != tc.want {
				t.Fatalf("outcome = %v, err = %v, want %v", res.Outcome, err, tc.want)
			}
			if got := rec.accept.count(); got != 1 {
				t.Errorf("accept observations = %d, want the accepted bid's 1 and nothing else", got)
			}
			if got := rec.lag.count(); got != 1 {
				t.Errorf("lag observations = %d, want the accepted bid's 1 and nothing else", got)
			}
		})
	}
}

// One successful commit of N bids observes the lag N times — the durability
// cost is per accepted bid — and the batch size once, whatever N turned out to
// be. The sum of the batch observations is therefore exactly the number of
// accepted bids, and that is the assertion that does not depend on how the
// accumulation loop happened to slice them.
func TestSuccessfulCommitObservesLagPerBidAndBatchPerCommit(t *testing.T) {
	const n = 40

	pg := testsupport.Start(t)
	rec := newRecorders()
	engine := New(pg.Pool, rec.observers())
	auctions := sameShardAuctions(t, pg.Pool, n, 0)

	// The warmup pays hydration for every auction, exactly as run-cell.sh does
	// before a measured cell, so the section below is batching and nothing else.
	for _, id := range auctions {
		res, err := engine.PlaceBid(context.Background(), bid.BidRequest{
			AuctionID: id, UserID: uuid.New(), AmountCents: minIncrement,
		})
		if err != nil || res.Outcome != bid.Accepted {
			t.Fatalf("warmup PlaceBid(%s): outcome %v, err %v", id, res.Outcome, err)
		}
	}

	var wg sync.WaitGroup
	for _, id := range auctions {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			res, err := engine.PlaceBid(context.Background(), bid.BidRequest{
				AuctionID: id, UserID: uuid.New(), AmountCents: 2 * minIncrement,
			})
			if err != nil || res.Outcome != bid.Accepted {
				t.Errorf("PlaceBid(%s): outcome %v, err %v", id, res.Outcome, err)
			}
		}(id)
	}
	wg.Wait()

	if got := rec.accept.count(); got != 2*n {
		t.Errorf("accept observations = %d, want one per accepted bid (%d)", got, 2*n)
	}
	if got := rec.lag.count(); got != 2*n {
		t.Errorf("lag observations = %d, want one per accepted bid (%d)", got, 2*n)
	}
	if got := rec.batch.sum(); got != float64(2*n) {
		t.Errorf("batch sizes sum to %v, want %d: every accepted bid rode exactly one commit", got, 2*n)
	}
	if got := rec.batch.count(); got >= 2*n {
		t.Errorf("batch observations = %d for %d accepted bids, want fewer: nothing grouped", got, 2*n)
	}
}

// The second writer of spec 01, reused: the batch aborts whole. shard_batch_size
// still sees it — it is the incident someone would use the series to explain
// (decisão 65) — and journal_lag_seconds does not, because a batch that aborted
// has no instant of durability.
func TestFailedCommitObservesBatchButNotLag(t *testing.T) {
	pg := testsupport.Start(t)
	rec := newRecorders()
	engine := New(pg.Pool, rec.observers())
	auction := seedAuction(t, pg.Pool, uuid.New(), time.Minute)

	if _, err := engine.PlaceBid(context.Background(), bid.BidRequest{
		AuctionID: auction, UserID: uuid.New(), AmountCents: minIncrement,
	}); err != nil {
		t.Fatalf("first bid: %v", err)
	}

	// A second writer takes seq 2 outside the shard's knowledge.
	if _, err := pg.Pool.Exec(context.Background(),
		`INSERT INTO bids (id, auction_id, user_id, amount_cents, seq) VALUES ($1, $2, $3, $4, 2)`,
		uuid.New(), auction, uuid.New(), 5000,
	); err != nil {
		t.Fatalf("plant seq 2: %v", err)
	}

	batchBefore, lagBefore := rec.batch.count(), rec.lag.count()

	if _, err := engine.PlaceBid(context.Background(), bid.BidRequest{
		AuctionID: auction, UserID: uuid.New(), AmountCents: 9000,
	}); err == nil {
		t.Fatal("PlaceBid after a foreign seq 2: got no error, want the batch to abort")
	}

	if got := rec.batch.count() - batchBefore; got != 1 {
		t.Errorf("batch observations across the aborted commit = %d, want 1", got)
	}
	if got := rec.lag.count() - lagBefore; got != 0 {
		t.Errorf("lag observations across the aborted commit = %d, want 0", got)
	}
	// The accept still happened: the shard decided, and only durability failed.
	if got := rec.accept.count(); got != 2 {
		t.Errorf("accept observations = %d, want 2", got)
	}
}

// What the collector in internal/metrics reads on every scrape: one depth per
// shard, in shard order, from a goroutine that is not the shard's.
func TestInboxDepthsReportsEveryShard(t *testing.T) {
	engine := New(nil, Observers{})

	depths := engine.InboxDepths()
	if len(depths) != numShards {
		t.Fatalf("InboxDepths() returned %d values, want %d", len(depths), numShards)
	}
	for i, d := range depths {
		if d != 0 {
			t.Errorf("shard %d depth = %d on an idle engine, want 0", i, d)
		}
	}
}
