// Package stream is the transport of the closing path: one Redis Stream, one
// consumer group, and the two ends that speak to it.
//
// It knows Redis and nothing else — no pgx, no internal/db, no
// internal/metrics. What travels is a uuid and nothing more: the database is
// the authority for every other fact about an auction (decisão 68), and a
// message carrying state would be a second source of truth riding a transport
// that delivers at-least-once.
package stream

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

const (
	// Key is the stream every expiry is published to, DeadKey the one that
	// receives what cannot be parsed, and Group the single consumer group.
	Key     = "auctions.expired"
	DeadKey = "auctions.expired.dead"
	Group   = "closerd"

	// field is the only field of an entry.
	field = "auction_id"

	// maxLen caps the stream, approximately, at the macro-node boundary. Redis
	// has 512M in the compose and shares it with the idempotency keys, which
	// are in the hot path of the three engines: a stream with no ceiling is a
	// hidden memory queue. A trimmed entry that was never consumed is not lost
	// — the sweep republishes it while the auction is still open and expired
	// (decisão 72).
	maxLen = 100_000
)

// Expired is the whole message: an id, and nothing else.
type Expired struct{ AuctionID uuid.UUID }

// Metrics is what the two ends feed, declared here and built in
// internal/metrics so the dependency points one way only — the same
// arrangement idem.Metrics has (decisão 28). The counters arrive already
// bound, so neither end can invent a series.
type Metrics struct {
	// Published counts entries the producer got into the stream.
	Published prometheus.Counter
	// Claimed counts entries XAUTOCLAIM took back from a consumer that stopped
	// answering. It leaving zero is the whole evidence that recovery ran.
	Claimed prometheus.Counter
	// Dead counts entries the consumer could not parse and buried.
	Dead prometheus.Counter
}

// Producer publishes expiries and reports the depth of the group.
//
// Both live on the same type on purpose: the queue is measured on the producer
// side (decisão 73), because a series that measures the consumer cannot live
// inside it — the target would stop being scraped exactly when the number
// starts to matter.
type Producer struct {
	rdb *redis.Client
	m   Metrics
}

// NewProducer wires the client and the observers. It touches Redis only when
// EnsureGroup is called.
func NewProducer(rdb *redis.Client, m Metrics) *Producer {
	return &Producer{rdb: rdb, m: m}
}

// EnsureGroup creates the group at the tail of the stream, treating BUSYGROUP
// as success.
//
// The producer creates it, and not the consumer (decisão 72): a group that only
// exists once the worker has booted makes every entry published before that
// invisible to it — a group created at $ sees only what arrives after — and
// leaves XINFO GROUPS, which is where the two gauges come from, with nothing to
// report.
//
// $ and not 0 for the reason the whole design rests on: the messages that
// matter describe a fact that is still true, and a fact that is still true
// comes back within a second (decisão 70). Replaying history would only produce
// a flood of already_closed.
func (p *Producer) EnsureGroup(ctx context.Context) error {
	err := p.rdb.XGroupCreateMkStream(ctx, Key, Group, "$").Err()
	if err == nil || strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return nil
	}
	return fmt.Errorf("create group %s on %s: %w", Group, Key, err)
}

// Publish writes one entry, and counts it only after Redis accepted it: a
// counter incremented before the XADD would report a queue that does not exist.
func (p *Producer) Publish(ctx context.Context, e Expired) error {
	err := p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: Key,
		MaxLen: maxLen,
		Approx: true,
		Values: map[string]any{field: e.AuctionID.String()},
	}).Err()
	if err != nil {
		return fmt.Errorf("publish expiry of %s: %w", e.AuctionID, err)
	}
	p.m.Published.Inc()
	return nil
}

// GroupInfo returns the two numbers that say different things about the same
// failure: pending is what was delivered and never acknowledged, backlog what
// was published and never delivered to anyone.
//
// A worker that was killed and did not come back produces no pending — it
// produces backlog. One series alone would draw an almost flat line through the
// whole outage (decisão 73).
//
// A missing group, or a lag Redis cannot compute, is an error and never
// (0, 0, nil): zero is a different claim from silence, and publishing zero
// queue while Redis is unreachable is the metric lying in the incident it
// exists for (decisão 59).
func (p *Producer) GroupInfo(ctx context.Context) (pending, backlog int64, err error) {
	groups, err := p.rdb.XInfoGroups(ctx, Key).Result()
	if err != nil {
		return 0, 0, fmt.Errorf("read groups of %s: %w", Key, err)
	}
	for _, g := range groups {
		if g.Name != Group {
			continue
		}
		if g.Lag < 0 {
			return 0, 0, fmt.Errorf("group %s: redis cannot compute the lag", Group)
		}
		return g.Pending, g.Lag, nil
	}
	return 0, 0, fmt.Errorf("group %s does not exist on %s", Group, Key)
}

// Name is the identity of this process inside the group.
//
// Two processes sharing one name would share one pending list, and each would
// claim the other's in-flight entry without it ever having been idle — which is
// the one thing XAUTOCLAIM must not be able to do.
func Name() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%s-%d", Group, host, os.Getpid())
}
