package closing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// Result is what one closing attempt turned out to be.
//
// RowsAffected() == 0 means three incompatible things, and each says something
// different about the system, so they are three values and not one noop
// (decisão 75). Summing them would make a reset between cells and a broken
// clock count in the same number.
type Result uint8

const (
	// Applied: the column was written by this attempt.
	Applied Result = iota
	// AlreadyClosed: a duplicate delivery, which is the normal behaviour of an
	// at-least-once transport, and the proof that the guard works.
	AlreadyClosed
	// Gone: the auction vanished between the publication and the consumption —
	// the TRUNCATE of the reset between cells (decisão 13), and nothing else in
	// this project.
	Gone
	// Early: a message describing a fact that is not true. It should be
	// impossible, since producer and consumer ask the same database for the
	// time; if it shows up, it is a finding about the system.
	Early
)

func (r Result) String() string {
	switch r {
	case Applied:
		return "applied"
	case AlreadyClosed:
		return "already_closed"
	case Gone:
		return "gone"
	case Early:
		return "early"
	}
	return "unknown"
}

// closeSQL is the whole worker, and both guards earn their place (decisão 68).
//
// status = 'open' is what makes the second delivery of the same message affect
// zero rows, so closed_at never moves: idempotence by construction, with no
// deduplication table and no key in Redis — the state the operation produces is
// the state that stops it from running again.
//
// ends_at <= clock_timestamp() is what makes this worker incapable of closing
// early, whatever the message says and whatever clock published it. Closing an
// auction too soon refuses a legitimate bid, which is the only direction of
// error worse than accepting a late one.
//
// Together they are why no sequence of kills, replays or fabricated ids can make
// this process violate an invariant. The worst it can do is not work.
const closeSQL = `UPDATE auctions
   SET status = 'closed', closed_at = clock_timestamp()
 WHERE id = $1 AND status = 'open' AND ends_at <= clock_timestamp()
RETURNING closed_at, extract(epoch from (closed_at - ends_at))`

// classifySQL runs only when nothing was updated — the single extra round-trip,
// and only on the cold path, which a healthy cell never takes.
const classifySQL = `SELECT status = 'closed', ends_at > clock_timestamp()
                       FROM auctions WHERE id = $1`

// MaterializerMetrics is what the worker feeds. The four counters arrive as
// bound children rather than as a CounterVec so a typo cannot publish a fifth
// result nobody would notice was wrong — the same reason idem.Metrics does it.
type MaterializerMetrics struct {
	Applied       prometheus.Counter
	AlreadyClosed prometheus.Counter
	Gone          prometheus.Counter
	Early         prometheus.Counter
	// Lag takes one sample per Applied, and the value comes from the database.
	Lag prometheus.Observer
}

// Materializer writes the closing. It holds no transaction, takes no lock and
// reads nothing beforehand: the winner is already in the row, written by the
// engines in the same statement that accepted the bid, and this stamps the end
// rather than deciding the result.
type Materializer struct {
	pool *pgxpool.Pool
	m    MaterializerMetrics
}

// NewMaterializer wires the pool and the observers.
func NewMaterializer(pool *pgxpool.Pool, m MaterializerMetrics) *Materializer {
	return &Materializer{pool: pool, m: m}
}

// Close runs the guarded UPDATE and classifies what came back.
//
// An infrastructure error returns an error, is not a Result and increments
// nothing: the caller leaves the entry unacknowledged and it comes back.
func (m *Materializer) Close(ctx context.Context, id uuid.UUID) (Result, error) {
	var closedAt time.Time
	var lag float64

	err := m.pool.QueryRow(ctx, closeSQL, id).Scan(&closedAt, &lag)
	switch {
	case err == nil:
		// The lag was computed by Postgres, inside the same statement, from two
		// columns written by its own clock. Subtracting a time.Now() of this
		// container would measure the closing plus the offset between two
		// clocks, and would do it silently (decisão 76).
		m.m.Lag.Observe(lag)
		m.m.Applied.Inc()
		return Applied, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("close auction %s: %w", id, err)
	}

	var closed, early bool
	if err := m.pool.QueryRow(ctx, classifySQL, id).Scan(&closed, &early); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			m.m.Gone.Inc()
			return Gone, nil
		}
		return 0, fmt.Errorf("classify auction %s: %w", id, err)
	}

	switch {
	case closed:
		m.m.AlreadyClosed.Inc()
		return AlreadyClosed, nil
	case early:
		m.m.Early.Inc()
		return Early, nil
	}

	// Open and expired, yet the UPDATE matched nothing: another transaction was
	// holding the row between the two statements. Nothing is counted and the
	// entry is not acknowledged, so it comes back — which is the right answer
	// to a race, and a wrong one to hide inside already_closed.
	return 0, fmt.Errorf("close auction %s: still open and expired after an update that matched no row", id)
}
