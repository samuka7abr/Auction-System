package metrics_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/samuka7abr/bid-storm/internal/metrics"
)

// The seven series of etapa 4 have to exist before the first closing: a
// /metrics that only grows a series once it has something to say makes an empty
// panel indistinguishable from a broken one, and gives a checkpoint nothing to
// verify before spending a cell.
func TestClosingSeriesExistBeforeAnyClosing(t *testing.T) {
	registry := metrics.NewRegistry()
	metrics.NewMaterializer(registry)
	metrics.NewStream(registry)
	metrics.RegisterStreamGroup(registry, &fakeGroup{})

	families := gather(t, registry)

	for _, name := range []string{
		"closing_events_published_total", "stream_claimed_total", "stream_dead_total",
	} {
		f := families[name]
		if f == nil {
			t.Fatalf("%s is missing from a registry that has published nothing", name)
		}
		if got := f.GetMetric()[0].GetCounter().GetValue(); got != 0 {
			t.Errorf("%s = %v, want 0", name, got)
		}
		if got := f.GetMetric()[0].GetLabel(); len(got) != 0 {
			t.Errorf("%s labels = %v, want none", name, got)
		}
	}

	lag := families["auction_close_lag_seconds"]
	if lag == nil {
		t.Fatal("auction_close_lag_seconds is missing")
	}
	if got := lag.GetMetric()[0].GetHistogram().GetSampleCount(); got != 0 {
		t.Errorf("auction_close_lag_seconds count = %d, want 0", got)
	}

	for _, name := range []string{"stream_pending_entries", "stream_backlog_entries"} {
		f := families[name]
		if f == nil {
			t.Fatalf("%s is missing: the two gauges are published by the producer, not the worker", name)
		}
		if got := f.GetMetric()[0].GetGauge().GetValue(); got != 0 {
			t.Errorf("%s = %v, want 0", name, got)
		}
	}
}

// The four values of result are bound at boot, so a worker that closed nothing
// still says so in four series instead of in none (decisão 59). Summing them
// into one noop would make a reset between cells and a broken clock count in
// the same number (decisão 75).
func TestTheFourResultsArePreBoundAndZeroed(t *testing.T) {
	registry := metrics.NewRegistry()
	metrics.NewMaterializer(registry)

	family := gather(t, registry)["auction_closings_total"]
	if family == nil {
		t.Fatal("auction_closings_total is missing")
	}

	var got []string
	for _, m := range family.GetMetric() {
		l := labels(m)
		if len(l) != 1 {
			t.Errorf("auction_closings_total labels = %v, want only result", l)
		}
		got = append(got, l["result"])
		if v := m.GetCounter().GetValue(); v != 0 {
			t.Errorf("auction_closings_total{result=%q} = %v, want 0", l["result"], v)
		}
	}
	slices.Sort(got)
	want := []string{"already_closed", "applied", "early", "gone"}
	if !slices.Equal(got, want) {
		t.Errorf("results = %v, want %v", got, want)
	}
}

// The buckets are a scale of their own, and share no boundary with confirm:
// this series is read against no other, and sharing would pile everything into
// the +Inf of one and the first bucket of the other (decisão 76).
func TestCloseLagBucketsAreItsOwnScale(t *testing.T) {
	registry := metrics.NewRegistry()
	m := metrics.NewMaterializer(registry)
	m.Lag.Observe(0.4)

	got := bounds(gather(t, registry)["auction_close_lag_seconds"].GetMetric()[0].GetHistogram())
	want := []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 300}
	if !slices.Equal(got, want) {
		t.Errorf("auction_close_lag_seconds boundaries = %v, want %v", got, want)
	}
}

// The collector keeps no state: what the group reports at the instant of the
// scrape is what comes out.
func TestStreamGroupCollectorReadsTheSourceOnEveryGather(t *testing.T) {
	registry := metrics.NewRegistry()
	src := &fakeGroup{pending: 2, backlog: 7}
	metrics.RegisterStreamGroup(registry, src)

	if p, b := queue(t, registry); p != 2 || b != 7 {
		t.Fatalf("first gather = (%v, %v), want (2, 7)", p, b)
	}

	src.pending, src.backlog = 0, 41
	if p, b := queue(t, registry); p != 0 || b != 41 {
		t.Errorf("second gather = (%v, %v), want (0, 41): the collector froze its first read", p, b)
	}
}

// An unreachable Redis, or a lag it cannot compute, does not become zero. Zero
// is a different claim from silence, and publishing an empty queue during the
// incident the series exists for would be the metric lying (decisões 59 e 73).
func TestStreamGroupCollectorEmitsNothingOnError(t *testing.T) {
	registry := metrics.NewRegistry()
	src := &fakeGroup{pending: 3, backlog: 9}
	metrics.RegisterStreamGroup(registry, src)

	src.err = errors.New("redis is not answering")
	families := gather(t, registry)
	for _, name := range []string{"stream_pending_entries", "stream_backlog_entries"} {
		if f := families[name]; f != nil {
			t.Errorf("%s = %v during a failed scrape, want the series not to be emitted",
				name, f.GetMetric())
		}
	}

	src.err = nil
	if p, b := queue(t, registry); p != 3 || b != 9 {
		t.Errorf("gather after recovery = (%v, %v), want (3, 9)", p, b)
	}
}

func queue(t *testing.T, registry *prometheus.Registry) (pending, backlog float64) {
	t.Helper()
	families := gather(t, registry)
	for _, name := range []string{"stream_pending_entries", "stream_backlog_entries"} {
		if families[name] == nil {
			t.Fatalf("%s is missing", name)
		}
	}
	return families["stream_pending_entries"].GetMetric()[0].GetGauge().GetValue(),
		families["stream_backlog_entries"].GetMetric()[0].GetGauge().GetValue()
}

type fakeGroup struct {
	pending, backlog int64
	err              error
}

func (f *fakeGroup) GroupInfo(context.Context) (int64, int64, error) {
	return f.pending, f.backlog, f.err
}
