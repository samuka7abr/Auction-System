package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// cellTotals is the size of the cell just verified: what the green lines report,
// and what I5 compares against the client.
type cellTotals struct {
	Auctions        int64
	Bids            int64
	MaxSeq          int64
	BidsWithKey     int64
	DistinctBidKeys int64
}

const totalsSQL = `SELECT count(DISTINCT auction_id), count(*), coalesce(max(seq), 0),
                          count(idempotency_key), count(DISTINCT idempotency_key)
                     FROM bids`

// Every SQL invariant is a query that returns no row when the invariant holds,
// and one already-formatted line per offender when it does not. Keeping the
// formatting in SQL is what lets four different shapes share one runner — and it
// puts the numbers that denounce a violation next to the predicate that found
// it, instead of in a Scan block three files away.
type sqlInvariant struct {
	id    string
	name  string
	query string
	// summary fills the green line for the invariants where the size of the cell
	// is worth stating; nil everywhere else, because "OK" needs no evidence.
	summary func(cellTotals) string
}

var sqlInvariants = []sqlInvariant{
	{
		id:   "I1",
		name: "sequência densa por leilão",
		// count(*) = max(seq) with min(seq) = 1 is denser than "no repetition":
		// UNIQUE (auction_id, seq) already forbids repeats, so asserting that
		// would be confirming Postgres. The hole is what this is looking for.
		query: `
			SELECT format('leilão %s: %s lances, seq de %s a %s',
			              auction_id, count(*), min(seq), max(seq))
			  FROM bids
			 GROUP BY auction_id
			HAVING count(*) <> max(seq) OR min(seq) <> 1
			 ORDER BY 1
			 LIMIT 5`,
		summary: func(t cellTotals) string {
			return fmt.Sprintf("%s, %s", plural64(t.Auctions, "leilão", "leilões"), plural64(t.Bids, "lance", "lances"))
		},
	},
	{
		id:   "I2",
		name: "incremento respeitado",
		// Stronger than monotonicity, and deliberately so. Strictly increasing is
		// what the optimistic engine's WHERE already guarantees; the increment
		// rule is what the shard of etapa 3 — deciding in memory, with no WHERE
		// at all — can violate without leaving a hole in the sequence.
		query: `
			SELECT format('leilão %s, seq %s: %s centavos após %s, incremento %s',
			              auction_id, seq, amount_cents, prev, min_increment_cents)
			  FROM (
			      SELECT b.auction_id, b.seq, b.amount_cents, a.min_increment_cents,
			             lag(b.amount_cents) OVER (PARTITION BY b.auction_id ORDER BY b.seq) AS prev
			        FROM bids b
			        JOIN auctions a ON a.id = b.auction_id
			  ) t
			 WHERE prev IS NOT NULL
			   AND amount_cents < prev + min_increment_cents
			 ORDER BY auction_id, seq
			 LIMIT 5`,
	},
	{
		id:   "I3",
		name: "vencedor e versão coerentes",
		// version is compared against count(bids) rather than max(seq) on purpose:
		// against max(seq) this would restate I1 on a cell with a hole, and say
		// nothing about the published state diverging from the recorded history.
		query: `
			SELECT format('leilão %s: version %s x %s lances, topo %s x %s, vencedor %s x %s',
			              a.id, a.version, coalesce(b.n, 0),
			              a.highest_bid_cents, coalesce(b.top, -1),
			              coalesce(a.highest_bidder::text, 'null'),
			              coalesce(b.last_bidder::text, 'null'))
			  FROM auctions a
			  LEFT JOIN (
			      SELECT auction_id,
			             count(*)          AS n,
			             max(amount_cents) AS top,
			             (array_agg(user_id ORDER BY seq DESC))[1] AS last_bidder
			        FROM bids
			       GROUP BY auction_id
			  ) b ON b.auction_id = a.id
			 WHERE a.version <> coalesce(b.n, 0)
			    OR (b.n IS NOT NULL
			        AND (a.highest_bid_cents <> b.top
			             OR a.highest_bidder IS DISTINCT FROM b.last_bidder))
			 ORDER BY a.id
			 LIMIT 5`,
	},
	{
		id:   "I4",
		name: "nenhum lance após o fechamento",
		// Not vacuous: the accepting UPDATE guards on now() < ends_at and
		// bids.created_at defaults to now(), but the two are the same
		// transaction_timestamp() only inside the CTE. This compares what the
		// database decided against what the database wrote, on one clock
		// (decisão 22) — and it is the invariant the closerd of etapa 4 must not
		// be able to break.
		query: `
			SELECT format('lance %s em %s, leilão %s fechava em %s',
			              b.id, b.created_at, a.id, a.ends_at)
			  FROM bids b
			  JOIN auctions a ON a.id = b.auction_id
			 WHERE b.created_at > a.ends_at
			 ORDER BY b.created_at
			 LIMIT 5`,
	},
	{
		id:   "I7",
		name: "chaves idempotentes presentes e únicas",
		query: `
			SELECT format('linhas=%s com_chave=%s distintas=%s',
			              count(*), count(idempotency_key), count(DISTINCT idempotency_key))
			  FROM bids
			HAVING count(*) <> count(idempotency_key)
			    OR count(*) <> count(DISTINCT idempotency_key)`,
		summary: func(t cellTotals) string {
			return fmt.Sprintf("linhas=%d com_chave=%d distintas=%d",
				t.Bids, t.BidsWithKey, t.DistinctBidKeys)
		},
	},
}

// checkSQL runs the five invariants Postgres can prove on its own.
//
// A violation never stops the others: the cell's report has to say everything
// that is wrong at once. A query that errors does stop them, because it means
// the database is not answering and no verdict below it would be worth reading.
func checkSQL(ctx context.Context, pool *pgxpool.Pool) ([]finding, cellTotals, error) {
	var totals cellTotals
	if err := pool.QueryRow(ctx, totalsSQL).Scan(
		&totals.Auctions, &totals.Bids, &totals.MaxSeq, &totals.BidsWithKey, &totals.DistinctBidKeys,
	); err != nil {
		return nil, totals, fmt.Errorf("count the cell: %w", err)
	}

	findings := make([]finding, 0, len(sqlInvariants))
	for _, inv := range sqlInvariants {
		bad, err := offenders(ctx, pool, inv.query)
		if err != nil {
			return nil, totals, fmt.Errorf("%s: %w", inv.id, err)
		}
		f := finding{ID: inv.id, Name: inv.name, Verdict: verdictOK}
		switch {
		case len(bad) > 0:
			f.Verdict = verdictFail
			f.Detail = strings.Join(bad, " · ")
		case inv.summary != nil:
			f.Detail = inv.summary(totals)
		}
		findings = append(findings, f)
	}
	return findings, totals, nil
}

func offenders(ctx context.Context, pool *pgxpool.Pool, query string) ([]string, error) {
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, rows.Err()
}

func plural64(n int64, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
