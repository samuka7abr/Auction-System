package metrics_test

import (
	"context"
	"slices"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/samuka7abr/bid-storm/internal/bid"
	"github.com/samuka7abr/bid-storm/internal/metrics"
)

// A process that has not taken a bid yet must publish the four series anyway:
// an empty panel has to be distinguishable from a broken one, and it is what
// lets a checkpoint verify the instrumentation before spending a benchmark
// cell.
func TestShardSeriesExistBeforeAnyObservation(t *testing.T) {
	registry := metrics.NewRegistry()
	metrics.NewShard(registry)
	metrics.RegisterShardInbox(registry, fakeInbox{depths: make([]int, 8)})

	families := gather(t, registry)

	for _, name := range []string{
		"bid_accept_duration_seconds", "journal_lag_seconds", "shard_batch_size",
	} {
		f := families[name]
		if f == nil {
			t.Fatalf("%s is missing from a registry that has not observed anything", name)
		}
		if got := f.GetMetric()[0].GetHistogram().GetSampleCount(); got != 0 {
			t.Errorf("%s count = %d, want 0", name, got)
		}
	}

	depth := families["shard_inbox_depth"]
	if depth == nil {
		t.Fatal("shard_inbox_depth is missing")
	}
	if got := len(depth.GetMetric()); got != 8 {
		t.Fatalf("shard_inbox_depth has %d series, want 8 — the zeroed shards have to show up too", got)
	}
	for i, m := range depth.GetMetric() {
		if got := labels(m)["shard"]; got != strconv.Itoa(i) {
			t.Errorf("series %d has shard=%q, want %q", i, got, strconv.Itoa(i))
		}
		if got := m.GetGauge().GetValue(); got != 0 {
			t.Errorf("shard %d depth = %v, want 0", i, got)
		}
	}
}

// The same assertion decisão 26 already demands of lock_wait_duration_seconds,
// and for the same reason: a later edit that splits the buckets would turn the
// reading against confirm into quantile interpolation, in silence. So every
// boundary confirm has is checked one by one — plus the six below 1ms, where
// confirm has nothing to say and the in-memory decision lives.
func TestShardHistogramsExtendConfirmDownwards(t *testing.T) {
	registry := metrics.NewRegistry()
	accept, lag, _ := metrics.NewShard(registry)

	// The decorator is what publishes bid_confirm_duration_seconds, and the
	// series sharing one registry is the arrangement under test.
	engine := metrics.Instrument(&fakeEngine{outcome: bid.Accepted}, registry, "shard")
	if _, err := engine.PlaceBid(context.Background(), bid.BidRequest{}); err != nil {
		t.Fatalf("PlaceBid: %v", err)
	}
	accept.Observe(0.000_02)
	lag.Observe(0.002)

	families := gather(t, registry)
	confirmBounds := bounds(families["bid_confirm_duration_seconds"].GetMetric()[0].GetHistogram())

	for _, name := range []string{"bid_accept_duration_seconds", "journal_lag_seconds"} {
		got := bounds(families[name].GetMetric()[0].GetHistogram())

		for _, want := range confirmBounds {
			if !slices.Contains(got, want) {
				t.Errorf("%s has no boundary %v, which confirm has: the two stop being readable against each other", name, want)
			}
		}
		if !slices.IsSorted(got) {
			t.Errorf("%s boundaries are not sorted: %v", name, got)
		}
		if len(got) != len(confirmBounds)+6 {
			t.Errorf("%s has %d boundaries, want confirm's %d plus the six below 1ms", name, len(got), len(confirmBounds))
		}
		if got[0] != 0.000_01 {
			t.Errorf("%s smallest boundary = %v, want 10µs", name, got[0])
		}
	}
}

// No strategy label on any of the four: in the other two engines the decision
// and the durability are the same statement, and a label with a single value
// would read as the other two reporting zero when they report nothing at all
// (decisão 59). shard_inbox_depth carries shard, and nothing else does
// (decisão 64).
func TestShardSeriesLabels(t *testing.T) {
	registry := metrics.NewRegistry()
	metrics.NewShard(registry)
	metrics.RegisterShardInbox(registry, fakeInbox{depths: make([]int, 8)})

	families := gather(t, registry)
	for _, name := range []string{
		"bid_accept_duration_seconds", "journal_lag_seconds", "shard_batch_size",
	} {
		if got := families[name].GetMetric()[0].GetLabel(); len(got) != 0 {
			t.Errorf("%s labels = %v, want none", name, got)
		}
	}
	for _, m := range families["shard_inbox_depth"].GetMetric() {
		got := labels(m)
		if len(got) != 1 {
			t.Errorf("shard_inbox_depth labels = %v, want only shard", got)
		}
		if _, ok := got["shard"]; !ok {
			t.Errorf("shard_inbox_depth labels = %v, want shard", got)
		}
	}
}

// shard_batch_size answers "how many bids fit in one commit", and the le="1"
// boundary is what makes the claim of decisão 48 readable as a plain ratio.
func TestBatchSizeBucketsReachTheBatchCeiling(t *testing.T) {
	registry := metrics.NewRegistry()
	_, _, batch := metrics.NewShard(registry)
	batch.Observe(1)

	got := bounds(gather(t, registry)["shard_batch_size"].GetMetric()[0].GetHistogram())
	want := []float64{1, 2, 4, 8, 16, 32, 64, 128, 256}
	if !slices.Equal(got, want) {
		t.Errorf("shard_batch_size boundaries = %v, want %v", got, want)
	}
}

// The collector keeps no state: whatever the engine reports at the instant of
// the scrape is what comes out, which is the whole point of not paying an
// Inc/Dec per bid to get it (decisão 63).
func TestInboxCollectorReadsTheSourceOnEveryGather(t *testing.T) {
	registry := metrics.NewRegistry()
	src := &movingInbox{depths: []int{0, 7, 0, 0, 0, 0, 0, 0}}
	metrics.RegisterShardInbox(registry, src)

	if got := depths(t, registry); !slices.Equal(got, []float64{0, 7, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("first gather = %v, want the source's 0,7,0...", got)
	}

	src.depths = []int{3, 0, 0, 0, 0, 0, 0, 0}
	if got := depths(t, registry); !slices.Equal(got, []float64{3, 0, 0, 0, 0, 0, 0, 0}) {
		t.Errorf("second gather = %v, want the new 3,0,0... — the collector froze its first read", got)
	}
}

type fakeInbox struct{ depths []int }

func (f fakeInbox) InboxDepths() []int { return f.depths }

type movingInbox struct{ depths []int }

func (m *movingInbox) InboxDepths() []int { return m.depths }

// depths returns shard_inbox_depth in shard order.
func depths(t *testing.T, registry *prometheus.Registry) []float64 {
	t.Helper()
	series := gather(t, registry)["shard_inbox_depth"].GetMetric()
	got := make([]float64, len(series))
	for i, m := range series {
		got[i] = m.GetGauge().GetValue()
	}
	return got
}
