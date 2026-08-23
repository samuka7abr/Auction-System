package closing

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/samuka7abr/bid-storm/internal/stream"
)

// The sweep publishes a fact, not an event: what is open and already past
// ends_at, and nothing else. An auction that has not expired and one already
// closed are both silent — the first because the fact is not true yet, the
// second because the column already says it.
func TestScanPublishesOnlyWhatIsOpenAndExpired(t *testing.T) {
	pool := freshDB(t)
	expired := insertAuction(t, pool, "-1 minute", "open")
	insertAuction(t, pool, "1 hour", "open")
	closedAndExpired := insertAuction(t, pool, "-1 minute", "closed")

	pub := &fakePublisher{}
	e := NewExpirer(pool, pub, discard())
	if err := e.scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(pub.sent) != 1 || pub.sent[0] != expired {
		t.Fatalf("published %v, want only the open and expired auction %s (never %s)",
			pub.sent, expired, closedAndExpired)
	}
}

// Republishing is what heals a lost entry, a restarted Redis and a recreated
// group with no recovery code at all — and the window is what keeps a stopped
// closerd from accumulating one copy per second (decisão 70).
func TestScanSuppressesInsideTheWindowAndRepublishesAfterIt(t *testing.T) {
	pool := freshDB(t)
	id := insertAuction(t, pool, "-1 minute", "open")

	pub := &fakePublisher{}
	e := NewExpirer(pool, pub, discard())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := e.scan(ctx); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
	}
	if len(pub.sent) != 1 {
		t.Fatalf("published %d times inside the window, want 1", len(pub.sent))
	}

	// Aged past the window, without waiting thirty seconds for it.
	e.published[id] = time.Now().Add(-suppressFor - time.Second)
	if err := e.scan(ctx); err != nil {
		t.Fatalf("scan after the window: %v", err)
	}
	if len(pub.sent) != 2 {
		t.Fatalf("published %d times, want 2: the fact is still true and has to come back", len(pub.sent))
	}
	if _, kept := e.published[id]; !kept {
		t.Error("the id left the suppression map after being republished")
	}
}

// A publication that fails is not fatal and does not enter the window: the next
// pass, one second later, tries it again.
func TestPublishErrorNeitherStopsThePassNorPoisonsTheNextOne(t *testing.T) {
	pool := freshDB(t)
	first := insertAuction(t, pool, "-2 minutes", "open")
	second := insertAuction(t, pool, "-1 minute", "open")

	pub := &fakePublisher{fail: map[uuid.UUID]bool{first: true}}
	e := NewExpirer(pool, pub, discard())
	ctx := context.Background()

	if err := e.scan(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	// ORDER BY ends_at puts the failing one first; the second still went out.
	if len(pub.sent) != 1 || pub.sent[0] != second {
		t.Fatalf("published %v, want only %s: one failure must not abort the pass", pub.sent, second)
	}

	pub.fail = nil
	if err := e.scan(ctx); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if len(pub.sent) != 2 || pub.sent[1] != first {
		t.Fatalf("published %v, want %s on the retry: a failure must not enter the window", pub.sent, first)
	}
}

// The reset between cells flushes Redis, and a producer that only created the
// group at boot would publish into a groupless stream for the rest of its life:
// the entries would be invisible to a group created later at $, and the closerd
// would sit on NOGROUP forever. Every pass puts the group back before it
// publishes anything.
func TestScanRestoresTheGroupBeforePublishing(t *testing.T) {
	pool := freshDB(t)
	insertAuction(t, pool, "-1 minute", "open")

	pub := &fakePublisher{}
	e := NewExpirer(pool, pub, discard())
	if err := e.scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if pub.ensured != 1 {
		t.Errorf("EnsureGroup called %d times, want 1 per pass", pub.ensured)
	}

	// A group that cannot be created stops the pass: publishing into a stream
	// with no group loses the fact for a whole suppression window, and the next
	// tick is one second away.
	pub.groupErr = errors.New("redis is not answering")
	before := len(pub.sent)
	if err := e.scan(context.Background()); err == nil {
		t.Fatal("scan = nil with no group, want an error")
	}
	if len(pub.sent) != before {
		t.Errorf("published %d entries into a groupless stream, want none", len(pub.sent)-before)
	}
}

type fakePublisher struct {
	sent     []uuid.UUID
	fail     map[uuid.UUID]bool
	ensured  int
	groupErr error
}

func (f *fakePublisher) EnsureGroup(context.Context) error {
	f.ensured++
	return f.groupErr
}

func (f *fakePublisher) Publish(_ context.Context, e stream.Expired) error {
	if f.fail[e.AuctionID] {
		return errors.New("redis is not answering")
	}
	f.sent = append(f.sent, e.AuctionID)
	return nil
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }
