package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samuka7abr/bid-storm/internal/testsupport"
)

const cellBids = 20

func TestSQLInvariantsPassOnACoherentCell(t *testing.T) {
	pg := testsupport.Start(t)
	newCell(t, pg.Pool, cellBids)

	findings, totals, err := checkSQL(context.Background(), pg.Pool)
	if err != nil {
		t.Fatalf("checkSQL: %v", err)
	}
	if totals.Auctions != 1 || totals.Bids != cellBids || totals.MaxSeq != cellBids {
		t.Fatalf("totals = %+v, want 1 auction and %d bids", totals, cellBids)
	}
	for _, f := range findings {
		if f.Verdict != verdictOK {
			t.Errorf("%s = %s (%s), want OK", f.ID, f.Verdict, f.Detail)
		}
	}
}

// A checker tested only against a correct database can be green by accident: a
// wrong JOIN returns zero rows, and zero rows is exactly what it reads as
// "invariant respected". These five cases are the ones that matter — without
// them the whole project would trust a verifier that never failed anything.
func TestSQLInvariantsCatchPlantedViolations(t *testing.T) {
	pg := testsupport.Start(t)

	cases := []struct {
		target string
		plant  []string
	}{
		{
			// The hole in the sequence. version follows the count so that I3 stays
			// green and the failing invariant is unambiguous.
			target: "I1",
			plant: []string{
				`DELETE FROM bids WHERE seq = 10`,
				`UPDATE auctions SET version = version - 1`,
			},
		},
		{
			// Strictly increasing, and still below the increment: the shape only I2
			// catches, and the one the shard of etapa 3 can produce.
			target: "I2",
			plant:  []string{`UPDATE bids SET amount_cents = amount_cents - 50 WHERE seq = 10`},
		},
		{
			target: "I3",
			plant:  []string{`UPDATE auctions SET highest_bid_cents = highest_bid_cents + 1`},
		},
		{
			target: "I4",
			plant: []string{`UPDATE bids SET created_at = (SELECT ends_at FROM auctions) + interval '1 second'
			                  WHERE seq = 1`},
		},
		{
			target: "I7",
			plant:  []string{`UPDATE bids SET idempotency_key = NULL WHERE seq = 1`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			newCell(t, pg.Pool, cellBids)
			for _, stmt := range tc.plant {
				exec(t, pg.Pool, stmt)
			}

			findings, _, err := checkSQL(context.Background(), pg.Pool)
			if err != nil {
				t.Fatalf("checkSQL: %v", err)
			}
			for _, f := range findings {
				switch {
				case f.ID == tc.target && f.Verdict != verdictFail:
					t.Errorf("%s = %s, want FAIL", f.ID, f.Verdict)
				case f.ID == tc.target && f.Detail == "":
					t.Errorf("%s failed without naming the number that denounced it", f.ID)
				case f.ID != tc.target && f.Verdict != verdictOK:
					t.Errorf("%s = %s (%s), want OK: the plant should break %s alone",
						f.ID, f.Verdict, f.Detail, tc.target)
				}
			}
			if rep := summarize("planted", findings); rep.Exit != exitViolated {
				t.Errorf("exit = %d, want %d", rep.Exit, exitViolated)
			}
		})
	}
}

// newCell writes a coherent cell by hand rather than through the engine: the
// planted violations then break exactly one thing, and the test can say which
// invariant was supposed to notice.
func newCell(t *testing.T, pool *pgxpool.Pool, n int64) uuid.UUID {
	t.Helper()
	exec(t, pool, `TRUNCATE bids, auctions RESTART IDENTITY CASCADE`)

	auction := uuid.New()
	bidders := make([]uuid.UUID, n)
	for i := range bidders {
		bidders[i] = uuid.New()
	}

	exec(t, pool, `INSERT INTO auctions
	                   (id, title, highest_bid_cents, highest_bidder, min_increment_cents, version, ends_at)
	               VALUES ($1, 'cell', $2, $3, 100, $4, now() + interval '1 hour')`,
		auction, n*100, bidders[n-1], n)

	for seq := int64(1); seq <= n; seq++ {
		exec(t, pool, `INSERT INTO bids (id, auction_id, user_id, amount_cents, seq, idempotency_key)
		               VALUES ($1, $2, $3, $4, $5, $6)`,
			uuid.New(), auction, bidders[seq-1], seq*100, seq, uuid.New())
	}
	return auction
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
