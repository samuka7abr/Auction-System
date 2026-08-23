package closing

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/samuka7abr/bid-storm/internal/testsupport"
)

func TestCloseAppliesOnceAndTheReplayBouncesOffTheGuard(t *testing.T) {
	pool := freshDB(t)
	id := insertAuction(t, pool, "-1 minute", "open")
	m, counts := materializer(t, pool)
	ctx := context.Background()

	got, err := m.Close(ctx, id)
	if err != nil || got != Applied {
		t.Fatalf("Close = (%v, %v), want Applied", got, err)
	}
	status, closedAt := auctionState(t, pool, id)
	if status != "closed" || closedAt.IsZero() {
		t.Fatalf("status=%q closed_at=%v, want closed and a timestamp", status, closedAt)
	}
	if n := counts.lag.count(); n != 1 {
		t.Fatalf("auction_close_lag_seconds took %d samples, want 1", n)
	}
	if v := counts.lag.last(); v < 0 {
		t.Errorf("lag = %v, want a non-negative interval computed by the database", v)
	}

	// The second delivery of the same message is the normal behaviour of an
	// at-least-once transport, and the whole point of status = 'open' in the
	// WHERE: it writes nothing and closed_at does not move.
	got, err = m.Close(ctx, id)
	if err != nil || got != AlreadyClosed {
		t.Fatalf("replay = (%v, %v), want AlreadyClosed", got, err)
	}
	_, again := auctionState(t, pool, id)
	if !again.Equal(closedAt) {
		t.Errorf("closed_at moved from %v to %v on the replay", closedAt, again)
	}
	if n := counts.lag.count(); n != 1 {
		t.Errorf("auction_close_lag_seconds took %d samples, want still 1", n)
	}
	assertCounts(t, counts, map[string]float64{"applied": 1, "already_closed": 1})
}

func TestCloseClassifiesTheThreeColdOutcomes(t *testing.T) {
	pool := freshDB(t)
	notYet := insertAuction(t, pool, "1 hour", "open")
	m, counts := materializer(t, pool)
	ctx := context.Background()

	// gone: the auction vanished between the publication and the consumption,
	// which in this project is the TRUNCATE of the reset between cells.
	if got, err := m.Close(ctx, uuid.New()); err != nil || got != Gone {
		t.Fatalf("Close on a missing auction = (%v, %v), want Gone", got, err)
	}

	// early: a message describing a fact that is not true. The guard makes it
	// harmless, and the counter makes it visible.
	if got, err := m.Close(ctx, notYet); err != nil || got != Early {
		t.Fatalf("Close on an auction that has not expired = (%v, %v), want Early", got, err)
	}
	if status, closedAt := auctionState(t, pool, notYet); status != "open" || !closedAt.IsZero() {
		t.Fatalf("status=%q closed_at=%v, want the row untouched", status, closedAt)
	}
	if n := counts.lag.count(); n != 0 {
		t.Errorf("auction_close_lag_seconds took %d samples on the cold path, want 0", n)
	}
	assertCounts(t, counts, map[string]float64{"gone": 1, "early": 1})
}

// The guard is the whole idempotence, and it is the database that enforces it:
// a hundred simultaneous attempts on the same auction, and exactly one writes.
func TestConcurrentClosesApplyExactlyOnce(t *testing.T) {
	pool := freshDB(t)
	id := insertAuction(t, pool, "-1 minute", "open")
	m, counts := materializer(t, pool)

	const attempts = 100
	results := make([]Result, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = m.Close(context.Background(), id)
		}(i)
	}
	wg.Wait()

	applied := 0
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("attempt %d: %v", i, errs[i])
		}
		if r == Applied {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("%d attempts applied, want exactly 1", applied)
	}
	if n := counts.lag.count(); n != 1 {
		t.Errorf("auction_close_lag_seconds took %d samples, want 1", n)
	}
	assertCounts(t, counts, map[string]float64{"applied": 1, "already_closed": attempts - 1})
}

type closingCounts struct {
	byResult map[string]prometheus.Counter
	lag      *recorder
}

func materializer(t *testing.T, pool *pgxpool.Pool) (*Materializer, closingCounts) {
	t.Helper()
	counts := closingCounts{byResult: map[string]prometheus.Counter{}, lag: &recorder{}}
	counter := func(name string) prometheus.Counter {
		c := prometheus.NewCounter(prometheus.CounterOpts{Name: name})
		counts.byResult[name] = c
		return c
	}
	return NewMaterializer(pool, MaterializerMetrics{
		Applied:       counter("applied"),
		AlreadyClosed: counter("already_closed"),
		Gone:          counter("gone"),
		Early:         counter("early"),
		Lag:           counts.lag,
	}), counts
}

func assertCounts(t *testing.T, c closingCounts, want map[string]float64) {
	t.Helper()
	for name, counter := range c.byResult {
		if got := testutil.ToFloat64(counter); got != want[name] {
			t.Errorf("auction_closings_total{result=%q} = %v, want %v", name, got, want[name])
		}
	}
}

// recorder stands in for the histogram: the tests need the sample count, which
// is what separates "observed once" from "observed on every cold outcome too".
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

func (r *recorder) last() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.values) == 0 {
		return -1
	}
	return r.values[len(r.values)-1]
}

// freshDB gives every test the shape decisão 13 gives every cell: one clean
// state, so no query needs a filter and a forgotten filter cannot hide anything.
func freshDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testsupport.Start(t).Pool
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE bids, auctions RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

func insertAuction(t *testing.T, pool *pgxpool.Pool, endsIn, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	// closed_at is written together with the status so the row is coherent —
	// which is exactly what I8 checks in the checker.
	sql := fmt.Sprintf(`INSERT INTO auctions (id, title, status, ends_at, closed_at)
	                    VALUES ($1, 'test', $2::auction_status,
	                            now() + interval '%s',
	                            CASE WHEN $2::text = 'closed' THEN now() END)`, endsIn)
	if _, err := pool.Exec(context.Background(), sql, id, status); err != nil {
		t.Fatalf("insert auction: %v", err)
	}
	return id
}

func auctionState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (string, time.Time) {
	t.Helper()
	var status string
	var closedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT status, closed_at FROM auctions WHERE id = $1`, id).Scan(&status, &closedAt); err != nil {
		t.Fatalf("read auction: %v", err)
	}
	if closedAt == nil {
		return status, time.Time{}
	}
	return status, *closedAt
}
