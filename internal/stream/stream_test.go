package stream

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/samuka7abr/bid-storm/internal/testsupport"
)

// The tests live inside the package for one reason: the claim test has to watch
// XAUTOCLAIM fire, and the real min-idle is thirty seconds. Nothing here reads
// configuration — the window is shortened by the test and put back.
func withMinIdle(t *testing.T, d time.Duration) {
	t.Helper()
	previous := claimMinIdle
	claimMinIdle = d
	t.Cleanup(func() { claimMinIdle = previous })
}

func TestEnsureGroupIsIdempotent(t *testing.T) {
	rdb, p, _ := transport(t)

	if err := p.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("first EnsureGroup: %v", err)
	}
	// BUSYGROUP is success: every auctiond boot calls this, and the second one
	// must not abort a process because the first one already worked.
	if err := p.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("second EnsureGroup: %v, want nil (BUSYGROUP is not a failure)", err)
	}

	groups, err := rdb.XInfoGroups(context.Background(), Key).Result()
	if err != nil {
		t.Fatalf("XINFO GROUPS: %v", err)
	}
	if len(groups) != 1 || groups[0].Name != Group {
		t.Fatalf("groups = %+v, want exactly %q", groups, Group)
	}
}

func TestPublishWritesAReadableEntryAndCountsIt(t *testing.T) {
	rdb, p, m := transport(t)
	ctx := context.Background()
	if err := p.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	id := uuid.New()
	if err := p.Publish(ctx, Expired{AuctionID: id}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := testutil.ToFloat64(m.Published); got != 1 {
		t.Errorf("closing_events_published_total = %v, want 1", got)
	}

	streams, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: Group, Consumer: "probe", Streams: []string{Key, ">"}, Count: 10,
	}).Result()
	if err != nil {
		t.Fatalf("XREADGROUP: %v", err)
	}
	if len(streams) != 1 || len(streams[0].Messages) != 1 {
		t.Fatalf("read %+v, want one entry", streams)
	}
	if got := streams[0].Messages[0].Values[field]; got != id.String() {
		t.Errorf("entry carries %v, want the auction id %s", got, id)
	}
}

// The two gauges answer different questions, and this is where that stops being
// prose: nobody reading makes backlog grow while pending stays flat, and reading
// without acknowledging moves the number to the other series (decisão 73).
func TestGroupInfoSeparatesBacklogFromPending(t *testing.T) {
	rdb, p, _ := transport(t)
	ctx := context.Background()
	if err := p.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := p.Publish(ctx, Expired{AuctionID: uuid.New()}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	pending, backlog, err := p.GroupInfo(ctx)
	if err != nil {
		t.Fatalf("GroupInfo: %v", err)
	}
	if pending != 0 || backlog != 3 {
		t.Fatalf("pending=%d backlog=%d, want 0 and 3 while nobody reads", pending, backlog)
	}

	if _, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: Group, Consumer: "probe", Streams: []string{Key, ">"}, Count: 10,
	}).Result(); err != nil {
		t.Fatalf("XREADGROUP: %v", err)
	}
	pending, backlog, err = p.GroupInfo(ctx)
	if err != nil {
		t.Fatalf("GroupInfo after the read: %v", err)
	}
	if pending != 3 || backlog != 0 {
		t.Fatalf("pending=%d backlog=%d, want 3 and 0 after reading without acknowledging", pending, backlog)
	}
}

// Zero is a different claim from silence: a GroupInfo that answered (0, 0, nil)
// with no group would publish an empty queue while the transport is broken.
func TestGroupInfoWithoutAGroupIsAnErrorAndNotZero(t *testing.T) {
	_, p, _ := transport(t)

	pending, backlog, err := p.GroupInfo(context.Background())
	if err == nil {
		t.Fatalf("GroupInfo = (%d, %d, nil), want an error: there is no group", pending, backlog)
	}
}

func TestConsumerDeliversAcknowledgesAndDoesNotRedeliver(t *testing.T) {
	_, p, m := transport(t)
	ctx := context.Background()
	if err := p.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	id := uuid.New()
	if err := p.Publish(ctx, Expired{AuctionID: id}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got := consume(t, p.rdb, m, "worker-a", 1, func(context.Context, Expired) error { return nil })
	if len(got) != 1 || got[0] != id {
		t.Fatalf("delivered %v, want [%s]", got, id)
	}

	pending, backlog, err := p.GroupInfo(ctx)
	if err != nil {
		t.Fatalf("GroupInfo: %v", err)
	}
	if pending != 0 || backlog != 0 {
		t.Fatalf("pending=%d backlog=%d, want both zero after the ack", pending, backlog)
	}
	// A second consumer must find nothing: the acknowledged entry is done.
	if again := consume(t, p.rdb, m, "worker-b", 0, func(context.Context, Expired) error { return nil }); len(again) != 0 {
		t.Errorf("second consumer got %v, want nothing", again)
	}
}

// A handler that fails leaves the entry pending, which is the only path in this
// design that recycles — and the right one, since the failure worth retrying is
// the database not answering (decisão 78).
func TestHandlerErrorLeavesTheEntryPending(t *testing.T) {
	_, p, m := transport(t)
	ctx := context.Background()
	if err := p.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if err := p.Publish(ctx, Expired{AuctionID: uuid.New()}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	consume(t, p.rdb, m, "worker-a", 1, func(context.Context, Expired) error {
		return context.DeadlineExceeded
	})

	pending, _, err := p.GroupInfo(ctx)
	if err != nil {
		t.Fatalf("GroupInfo: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending = %d, want 1: an unacknowledged entry has to survive", pending)
	}
}

// The property the chaos scenario spends: a worker that died with the message in
// its hand leaves exactly one entry pending, and the next one takes it back.
func TestAnotherConsumerClaimsWhatADeadOneLeftPending(t *testing.T) {
	withMinIdle(t, 50*time.Millisecond)
	_, p, m := transport(t)
	ctx := context.Background()
	if err := p.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	id := uuid.New()
	if err := p.Publish(ctx, Expired{AuctionID: id}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// The dead worker: it read the entry and never acknowledged it.
	consume(t, p.rdb, m, "worker-dead", 1, func(context.Context, Expired) error {
		return context.DeadlineExceeded
	})
	if got := testutil.ToFloat64(m.Claimed); got != 0 {
		t.Fatalf("stream_claimed_total = %v before any claim, want 0", got)
	}

	time.Sleep(80 * time.Millisecond)
	got := consume(t, p.rdb, m, "worker-alive", 1, func(context.Context, Expired) error { return nil })
	if len(got) != 1 || got[0] != id {
		t.Fatalf("claimed %v, want [%s]", got, id)
	}
	if n := testutil.ToFloat64(m.Claimed); n < 1 {
		t.Errorf("stream_claimed_total = %v, want at least 1", n)
	}
	if pending, _, err := p.GroupInfo(ctx); err != nil || pending != 0 {
		t.Errorf("pending = %d (err %v), want 0 after the claim was acknowledged", pending, err)
	}
}

func TestMalformedPayloadIsBuriedAndAcknowledged(t *testing.T) {
	rdb, p, m := transport(t)
	ctx := context.Background()
	if err := p.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: Key, Values: map[string]any{field: "not-a-uuid"},
	}).Err(); err != nil {
		t.Fatalf("XADD: %v", err)
	}

	if got := consume(t, rdb, m, "worker-a", 0, func(context.Context, Expired) error {
		t.Error("the handler must not see an entry that is not a uuid")
		return nil
	}); len(got) != 0 {
		t.Fatalf("handler saw %v", got)
	}

	if n := testutil.ToFloat64(m.Dead); n != 1 {
		t.Errorf("stream_dead_total = %v, want 1", n)
	}
	if pending, _, err := p.GroupInfo(ctx); err != nil || pending != 0 {
		t.Errorf("pending = %d (err %v), want 0: a buried entry is acknowledged", pending, err)
	}
	buried, err := rdb.XLen(ctx, DeadKey).Result()
	if err != nil {
		t.Fatalf("XLEN of the dead letter: %v", err)
	}
	if buried != 1 {
		t.Errorf("%s holds %d entries, want 1", DeadKey, buried)
	}
}

// transport gives a clean stream, a producer and the counters behind it. The
// keys are deleted rather than the container recreated: one Redis per binary is
// what testsupport promises.
func transport(t *testing.T) (*redis.Client, *Producer, Metrics) {
	t.Helper()
	rdb := testsupport.StartRedis(t).Client
	if err := rdb.Del(context.Background(), Key, DeadKey).Err(); err != nil {
		t.Fatalf("clear the streams: %v", err)
	}
	m := Metrics{
		Published: prometheus.NewCounter(prometheus.CounterOpts{Name: "published"}),
		Claimed:   prometheus.NewCounter(prometheus.CounterOpts{Name: "claimed"}),
		Dead:      prometheus.NewCounter(prometheus.CounterOpts{Name: "dead"}),
	}
	return rdb, NewProducer(rdb, m), m
}

// consume runs one consumer until it has handled want entries, or until the
// read block has had its turn. It returns what the handler saw.
func consume(t *testing.T, rdb *redis.Client, m Metrics, name string, want int, h Handler) []uuid.UUID {
	t.Helper()

	seen := make(chan uuid.UUID, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := NewConsumer(rdb, name, m, slog.New(slog.DiscardHandler))
	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, func(ctx context.Context, e Expired) error {
			seen <- e.AuctionID
			return h(ctx, e)
		})
	}()

	var got []uuid.UUID
	deadline := time.After(3 * time.Second)
	for len(got) < want {
		select {
		case id := <-seen:
			got = append(got, id)
		case <-deadline:
			t.Fatalf("consumer %s handled %d entries, want %d", name, len(got), want)
		}
	}
	if want == 0 {
		// Nothing is expected, so give the loop one full turn before stopping it.
		time.Sleep(500 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case id := <-seen:
		got = append(got, id)
	default:
	}
	return got
}
