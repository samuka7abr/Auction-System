package metrics

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// shardBuckets is confirmBuckets extended downwards, never a separate scale.
//
// Copying confirmBuckets literally would not work: its floor is 1ms, and the
// in-memory decision of the shard lives three orders of magnitude below that,
// so every observation would land in the first bucket and the histogram would
// only ever say "less than a millisecond". Inventing a scale of its own would
// break decisão 26 the other way — the comparison against confirm would go
// through quantile interpolation, which is exactly where a difference of ten
// percentage points hides.
//
// The superset satisfies both: every boundary confirm has still exists, so
// reading bucket by bucket against it is exact everywhere confirm has anything
// to say, and below 1ms — where it has nothing — the new series carry their own
// resolution (decisão 61).
var shardBuckets = append(
	[]float64{0.000_01, 0.000_025, 0.000_05, 0.000_1, 0.000_25, 0.000_5},
	confirmBuckets...,
)

// NewShard registers the three histograms of the single-writer mechanism and
// returns the observers the shard engine feeds. Like NewLockWait, it lives here
// so every series name the process publishes stays in one package, and the
// engine is handed one-method interfaces instead (decisão 28).
//
// None of the three carries a strategy label. In the other two engines the
// decision and the durability happen inside the same statement, so feeding them
// bid_accept_duration_seconds would publish a copy of confirm under another
// name and a reader would conclude their durability is free, when it is
// indivisible. A label with a single value suggests the other two report zero,
// and zero is a different claim from silence (decisão 59).
func NewShard(reg prometheus.Registerer) (accept, lag, batch prometheus.Observer) {
	acceptH := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "bid_accept_duration_seconds",
		// From the first line of PlaceBid — the wait to enter the inbox is cost
		// of the mechanism, not noise before it (decisão 62).
		Help:    "Time from entering PlaceBid to the in-memory decision, observed only on accepts.",
		Buckets: shardBuckets,
	})

	// A histogram and not the gauge observabilidade.md announced: with the
	// synchronous commit of decisão 55 the window between decided and durable is
	// exactly one batch — hundreds of microseconds to a few milliseconds — and a
	// gauge read every 5 seconds would sample it almost always outside of it
	// (decisão 60).
	lagH := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "journal_lag_seconds",
		Help:    "Time from the in-memory decision to the durable commit, per accepted bid.",
		Buckets: shardBuckets,
	})

	batchH := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "shard_batch_size",
		Help: "Accepted bids per commit attempt.",
		// 1, 2, 4 ... 256, the batch ceiling of decisão 56. The le="1" boundary
		// is not decorative: it makes the claim of decisão 48 — on the
		// single-auction cell the batch is worth 1 — readable as a plain ratio,
		// with no quantile in the way.
		Buckets: prometheus.ExponentialBuckets(1, 2, 9),
	})

	reg.MustRegister(acceptH, lagH, batchH)
	return acceptH, lagH, batchH
}

// InboxDepths is what the engine exposes to the collector: one depth per shard,
// read at scrape time.
type InboxDepths interface{ InboxDepths() []int }

// RegisterShardInbox publishes shard_inbox_depth, sampled from src on every
// scrape.
//
// It is a separate function from NewShard because the order forces it: the
// observers exist before the engine, and the engine exists before the collector
// that reads it.
//
// A collector rather than an Inc on send and a Dec on receive: those are two
// atomic writes per bid on the hot path of the engine whose whole thesis is
// that its hot path is cheap — the measurement would finance the result. len
// over a channel is a read, safe from any goroutine, and Prometheus pays for it
// every 5 seconds instead of the bid paying for it (decisão 63).
func RegisterShardInbox(reg prometheus.Registerer, src InboxDepths) {
	reg.MustRegister(&inboxCollector{
		src: src,
		desc: prometheus.NewDesc(
			"shard_inbox_depth",
			"Commands queued for each shard goroutine at the instant of the scrape.",
			[]string{"shard"}, nil,
		),
	})
}

type inboxCollector struct {
	src  InboxDepths
	desc *prometheus.Desc
}

func (c *inboxCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect emits one series per shard on every scrape, including the ones worth
// zero: an empty panel has to be distinguishable from a broken one, and no
// state is kept between scrapes.
func (c *inboxCollector) Collect(ch chan<- prometheus.Metric) {
	for i, depth := range c.src.InboxDepths() {
		ch <- prometheus.MustNewConstMetric(
			c.desc, prometheus.GaugeValue, float64(depth), strconv.Itoa(i),
		)
	}
}
