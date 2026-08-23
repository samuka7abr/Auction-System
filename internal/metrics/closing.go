package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/samuka7abr/bid-storm/internal/closing"
	"github.com/samuka7abr/bid-storm/internal/stream"
)

// closeLagBuckets is a scale of its own, and deliberately shares no boundary
// with confirmBuckets.
//
// Decisão 26 demands shared boundaries between series that are read against
// each other, and this one is read against none: bid_confirm_duration_seconds
// lives in milliseconds and measures the hot path, while this lives in seconds
// and measures a worker that does not try to be fast. Sharing boundaries would
// only pile everything into the +Inf of one and the first bucket of the other
// (decisão 76).
var closeLagBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 300}

// NewMaterializer registers the two series of the worker and returns the
// observers it feeds.
//
// The four values of result are bound now, not on the first closing: a /metrics
// that has closed nothing still publishes the four at zero, so an empty panel
// stays distinguishable from a broken one (decisão 59).
func NewMaterializer(reg prometheus.Registerer) closing.MaterializerMetrics {
	closings := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "auction_closings_total",
		Help: "Closing attempts by outcome: applied, already_closed, gone or early.",
	}, []string{"result"})

	lag := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "auction_close_lag_seconds",
		// From when the auction should have been closed to when the column said
		// so. It includes the average half tick of the sweep, the trip through
		// Redis and the queue, on purpose: that is the operator's question.
		Help:    "Time between ends_at and the closed_at the database stamped.",
		Buckets: closeLagBuckets,
	})

	reg.MustRegister(closings, lag)

	return closing.MaterializerMetrics{
		Applied:       closings.WithLabelValues(closing.Applied.String()),
		AlreadyClosed: closings.WithLabelValues(closing.AlreadyClosed.String()),
		Gone:          closings.WithLabelValues(closing.Gone.String()),
		Early:         closings.WithLabelValues(closing.Early.String()),
		Lag:           lag,
	}
}

// NewStream registers the three counters of the transport and returns them
// already bound.
//
// One constructor for both ends because there is one transport: the producer
// feeds Published and the consumer feeds Claimed and Dead. Each process
// registers what it can feed and publishes the rest at zero, which is the same
// arrangement every other series in this project has at boot.
func NewStream(reg prometheus.Registerer) stream.Metrics {
	published := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "closing_events_published_total",
		Help: "Expiry events the sweep got into the stream.",
	})
	claimed := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "stream_claimed_total",
		Help: "Entries XAUTOCLAIM took back from a consumer that stopped answering.",
	})
	dead := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "stream_dead_total",
		Help: "Entries sent to the dead letter stream because the payload was not a uuid.",
	})

	reg.MustRegister(published, claimed, dead)
	return stream.Metrics{Published: published, Claimed: claimed, Dead: dead}
}

// StreamGroup is what the producer exposes to the collector, read at scrape
// time.
type StreamGroup interface {
	GroupInfo(ctx context.Context) (pending, backlog int64, err error)
}

// groupInfoTimeout caps one scrape's round-trip. It is well above the
// microseconds an XINFO GROUPS costs and well below the five seconds between
// scrapes, so a Redis that stopped answering delays nothing.
const groupInfoTimeout = 200 * time.Millisecond

// RegisterStreamGroup publishes the two queue gauges, sampled from src on every
// scrape.
//
// They live in the producer, and that is the most important design decision of
// this block (decisão 73): the series that measures the consumer cannot live
// inside it, or the target stops being scraped exactly when the number starts
// to matter, and the chart of the chaos scenario gets a hole where the evidence
// should be.
func RegisterStreamGroup(reg prometheus.Registerer, src StreamGroup) {
	reg.MustRegister(&streamGroupCollector{
		src: src,
		pending: prometheus.NewDesc(
			"stream_pending_entries",
			"Entries delivered to the group and not acknowledged.",
			nil, nil),
		backlog: prometheus.NewDesc(
			"stream_backlog_entries",
			"Entries published and never delivered to any consumer.",
			nil, nil),
	})
}

type streamGroupCollector struct {
	src     StreamGroup
	pending *prometheus.Desc
	backlog *prometheus.Desc
}

func (c *streamGroupCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pending
	ch <- c.backlog
}

// Collect emits both gauges, or neither.
//
// An error — Redis unreachable, or a lag it cannot compute after certain trims
// — does not become zero: the series is simply not emitted in that scrape.
// Publishing zero queue while Redis is down would be the metric lying in the
// incident it exists for (decisão 59).
func (c *streamGroupCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), groupInfoTimeout)
	defer cancel()

	pending, backlog, err := c.src.GroupInfo(ctx)
	if err != nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(pending))
	ch <- prometheus.MustNewConstMetric(c.backlog, prometheus.GaugeValue, float64(backlog))
}
