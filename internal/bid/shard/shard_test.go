package shard

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samuka7abr/bid-storm/internal/bid"
	"github.com/samuka7abr/bid-storm/internal/bid/enginetest"
	"github.com/samuka7abr/bid-storm/internal/testsupport"
)

const minIncrement = 100

// The whole cost of the third engine, in test terms: the suite is written
// against BidEngine and never against SQL, so an engine that decides entirely
// in memory passes it or is wrong (decisão 11).
func TestConformance(t *testing.T) {
	enginetest.RunConformance(t, func(pool *pgxpool.Pool) bid.BidEngine {
		return New(pool)
	})
}

// The invariant of method this engine could break for free: nothing in the
// code stops PlaceBid from answering before the commit that makes the answer
// true. This is the assertion that would catch it (decisão 8).
func TestAcceptedBidIsVisibleBeforePlaceBidReturns(t *testing.T) {
	pg := testsupport.Start(t)
	engine := New(pg.Pool)
	auction := seedAuction(t, pg.Pool, uuid.New(), time.Minute)

	res, err := engine.PlaceBid(context.Background(), bid.BidRequest{
		AuctionID: auction, UserID: uuid.New(), AmountCents: minIncrement,
	})
	if err != nil || res.Outcome != bid.Accepted {
		t.Fatalf("PlaceBid: outcome %v, err %v", res.Outcome, err)
	}

	// A connection of its own, acquired after PlaceBid already returned: if the
	// row is not there yet, the 201 was a promise the commit had not kept.
	conn, err := pg.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	var count int
	if err := conn.QueryRow(context.Background(),
		`SELECT count(*) FROM bids WHERE auction_id = $1 AND seq = $2`, auction, res.Seq,
	).Scan(&count); err != nil {
		t.Fatalf("count bids: %v", err)
	}
	if count != 1 {
		t.Errorf("bids rows = %d, want 1: PlaceBid answered before the commit was durable", count)
	}
}

// This engine never reads ExpectedVersion: a stale version with a large enough
// amount is accepted, and so is no version at all. On the optimistic engine
// the same two requests are 409 and 400.
func TestExpectedVersionIsIgnored(t *testing.T) {
	pg := testsupport.Start(t)
	engine := New(pg.Pool)

	t.Run("stale version with enough amount is accepted", func(t *testing.T) {
		auction := seedAuction(t, pg.Pool, uuid.New(), time.Minute)
		zero := int64(0)

		first, err := engine.PlaceBid(context.Background(), bid.BidRequest{
			AuctionID: auction, UserID: uuid.New(), AmountCents: minIncrement, ExpectedVersion: &zero,
		})
		if err != nil || first.Outcome != bid.Accepted {
			t.Fatalf("first bid: outcome %v, err %v", first.Outcome, err)
		}

		res, err := engine.PlaceBid(context.Background(), bid.BidRequest{
			AuctionID: auction, UserID: uuid.New(), AmountCents: 9 * minIncrement, ExpectedVersion: &zero,
		})
		if err != nil {
			t.Fatalf("PlaceBid: %v", err)
		}
		if res.Outcome != bid.Accepted {
			t.Fatalf("outcome = %v, want accepted", res.Outcome)
		}
	})

	t.Run("nil expected version is accepted", func(t *testing.T) {
		auction := seedAuction(t, pg.Pool, uuid.New(), time.Minute)

		res, err := engine.PlaceBid(context.Background(), bid.BidRequest{
			AuctionID: auction, UserID: uuid.New(), AmountCents: minIncrement,
		})
		if err != nil {
			t.Fatalf("PlaceBid: %v", err)
		}
		if res.Outcome != bid.Accepted {
			t.Fatalf("outcome = %v, want accepted", res.Outcome)
		}
	})
}

// What only a single-writer engine can prove: not one result out of a genuine
// pile-up is Conflict or Invalid, because there is no version to read and
// nothing to fail against — the loser gets TooLow, decided in memory.
func TestConcurrencyProducesNeitherConflictNorInvalid(t *testing.T) {
	const workers, attempts = 8, 10

	pg := testsupport.Start(t)
	engine := New(pg.Pool)
	auction := seedAuction(t, pg.Pool, uuid.New(), time.Minute)

	outcomes := make(chan bid.Outcome, workers*attempts)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			amount := int64(minIncrement)
			for range attempts {
				res, err := engine.PlaceBid(context.Background(), bid.BidRequest{
					AuctionID: auction, UserID: uuid.New(), AmountCents: amount,
				})
				if err != nil {
					t.Errorf("PlaceBid: %v", err)
					return
				}
				outcomes <- res.Outcome
				amount = res.Current.MinNextBid()
			}
		}()
	}
	wg.Wait()
	close(outcomes)

	counts := map[bid.Outcome]int{}
	for o := range outcomes {
		counts[o]++
	}
	if counts[bid.Conflict] != 0 || counts[bid.Invalid] != 0 {
		t.Errorf("conflict = %d, invalid = %d, want zero of each: this engine reads no version",
			counts[bid.Conflict], counts[bid.Invalid])
	}
	if counts[bid.Accepted] == 0 {
		t.Fatal("nothing was accepted: the case proved nothing")
	}
	if counts[bid.TooLow] == 0 {
		t.Error("nothing was too low: the workers never collided, so the case proved nothing")
	}
}

// The mechanism the engine exists to prove: bids accepted for distinct
// auctions on the same shard, fired together, commit in fewer transactions
// than there are accepted bids (decisão 48).
func TestBatchCommitsMoreThanOneBidAtOnce(t *testing.T) {
	const n = 40

	pg := testsupport.Start(t)
	engine := New(pg.Pool)
	auctions := sameShardAuctions(t, pg.Pool, n, 0)

	// Warm up hydration for every auction with a first bid, exactly as
	// run-cell.sh's warmup pays the one round-trip per auction before the
	// measured cell (decisão 49) — so the section below isolates batching.
	for _, id := range auctions {
		res, err := engine.PlaceBid(context.Background(), bid.BidRequest{
			AuctionID: id, UserID: uuid.New(), AmountCents: minIncrement,
		})
		if err != nil || res.Outcome != bid.Accepted {
			t.Fatalf("warmup PlaceBid(%s): outcome %v, err %v", id, res.Outcome, err)
		}
	}

	before := xactCommits(t, pg.Pool)

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

	after := xactCommits(t, pg.Pool)
	if got := after - before; got >= n {
		t.Errorf("transactions = %d for %d accepted bids fired together, want fewer: the batch never grouped anything",
			got, n)
	}
}

// A second writer commits a bid the shard never decided. The shard's next
// commit for that auction collides on UNIQUE (auction_id, seq), aborts whole,
// and the following command re-hydrates and lands on the correct seq — the
// shard loses authority instead of lying (decisão 53).
func TestSecondWriterAbortsCommitAndShardRecovers(t *testing.T) {
	pg := testsupport.Start(t)
	engine := New(pg.Pool)
	auction := seedAuction(t, pg.Pool, uuid.New(), time.Minute)

	first, err := engine.PlaceBid(context.Background(), bid.BidRequest{
		AuctionID: auction, UserID: uuid.New(), AmountCents: minIncrement,
	})
	if err != nil || first.Outcome != bid.Accepted || first.Seq != 1 {
		t.Fatalf("first bid: seq %d, outcome %v, err %v", first.Seq, first.Outcome, err)
	}

	// A second writer takes seq 2 outside the shard's knowledge, coherent with
	// the auctions row it also updates.
	other := uuid.New()
	if _, err := pg.Pool.Exec(context.Background(),
		`INSERT INTO bids (id, auction_id, user_id, amount_cents, seq) VALUES ($1, $2, $3, $4, 2)`,
		uuid.New(), auction, other, 5000,
	); err != nil {
		t.Fatalf("plant seq 2: %v", err)
	}
	if _, err := pg.Pool.Exec(context.Background(),
		`UPDATE auctions SET highest_bid_cents = 5000, highest_bidder = $2, version = 2 WHERE id = $1`,
		auction, other,
	); err != nil {
		t.Fatalf("plant auction update: %v", err)
	}

	// The shard still believes the next seq is 2: its statement collides with
	// the row planted above and the whole batch aborts.
	if _, err := engine.PlaceBid(context.Background(), bid.BidRequest{
		AuctionID: auction, UserID: uuid.New(), AmountCents: 9000,
	}); err == nil {
		t.Fatal("PlaceBid after a foreign seq 2: got no error, want the batch to abort")
	}

	// Re-hydration reads the truth back, without restarting the process.
	recovered, err := engine.PlaceBid(context.Background(), bid.BidRequest{
		AuctionID: auction, UserID: uuid.New(), AmountCents: 9000,
	})
	if err != nil {
		t.Fatalf("PlaceBid after recovery: %v", err)
	}
	if recovered.Outcome != bid.Accepted || recovered.Seq != 3 {
		t.Fatalf("recovered outcome = %v, seq = %d, want accepted at seq 3", recovered.Outcome, recovered.Seq)
	}

	var version, highest int64
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT version, highest_bid_cents FROM auctions WHERE id = $1`, auction,
	).Scan(&version, &highest); err != nil {
		t.Fatalf("read auction: %v", err)
	}
	if version != 3 || highest != 9000 {
		t.Errorf("auction = (version %d, highest %d), want (3, 9000)", version, highest)
	}
}

// The map starts empty at New, so any auction seeded after the engine is built
// — every auction in this suite — is only reachable through the hydration
// path (decisão 49).
func TestAuctionCreatedAfterBootIsHydrated(t *testing.T) {
	pg := testsupport.Start(t)
	engine := New(pg.Pool) // boot: no DB touched yet

	auction := seedAuction(t, pg.Pool, uuid.New(), time.Minute) // created well after

	res, err := engine.PlaceBid(context.Background(), bid.BidRequest{
		AuctionID: auction, UserID: uuid.New(), AmountCents: minIncrement,
	})
	if err != nil {
		t.Fatalf("PlaceBid: %v", err)
	}
	if res.Outcome != bid.Accepted || res.Seq != 1 {
		t.Fatalf("outcome = %v, seq = %d, want accepted at seq 1", res.Outcome, res.Seq)
	}
}

func seedAuction(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, endsIn time.Duration) uuid.UUID {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO auctions (id, title, min_increment_cents, ends_at)
		 VALUES ($1, $2, $3, now() + make_interval(secs => $4))`,
		id, t.Name(), minIncrement, endsIn.Seconds(),
	); err != nil {
		t.Fatalf("seed auction: %v", err)
	}
	return id
}

// sameShardAuctions seeds n auctions that shardFor routes to target — the only
// way to make several auctions collide in one goroutine's batch on purpose.
func sameShardAuctions(t *testing.T, pool *pgxpool.Pool, n, target int) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, 0, n)
	for len(ids) < n {
		id := uuid.New()
		if shardFor(id, numShards) != target {
			continue
		}
		ids = append(ids, seedAuction(t, pool, id, time.Minute))
	}
	return ids
}

func xactCommits(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT xact_commit FROM pg_stat_database WHERE datname = current_database()`,
	).Scan(&n); err != nil {
		t.Fatalf("read xact_commit: %v", err)
	}
	return n
}
