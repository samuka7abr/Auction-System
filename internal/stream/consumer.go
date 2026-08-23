package stream

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Handler materialises one expiry. Returning an error leaves the entry
// unacknowledged, which is the only way an entry survives to be retried.
type Handler func(ctx context.Context, e Expired) error

var (
	// claimMinIdle separates "the worker is slow" from "the worker died with
	// the message in its hand". A normal closing takes milliseconds, so thirty
	// seconds is orders of magnitude above it and still short enough for the
	// chaos scenario to watch the recovery happen (decisão 78). It is a var
	// only so the transport test can watch a claim without sleeping for half a
	// minute; nothing reads it from the environment.
	claimMinIdle = 30 * time.Second

	// readBlock is how long XREADGROUP parks before the loop comes back around
	// to XAUTOCLAIM.
	readBlock = 5 * time.Second

	// retryPause keeps an unreachable Redis from turning the loop into a spin.
	retryPause = time.Second
)

// readCount caps one read. It is not a batch: the messages are processed one at
// a time, in sequence (decisão 77).
const readCount = 64

// messageGrace is how long an in-flight message gets after the process was told
// to stop. `docker compose stop` is a clean shutdown and has to finish the
// message in hand; `docker kill` is the chaos scenario and gets none of this.
const messageGrace = 5 * time.Second

// Consumer is the single reader of the group: one process, one loop, one
// message in flight.
//
// That last property is what the chaos scenario spends: at any instant at most
// one entry is unacknowledged, so killing the process leaves at most one entry
// in the pending list, and what XAUTOCLAIM recovers is exactly that one.
type Consumer struct {
	rdb  *redis.Client
	name string
	m    Metrics
	log  *slog.Logger
}

// NewConsumer builds the reader. name is the identity inside the group — see
// Name.
func NewConsumer(rdb *redis.Client, name string, m Metrics, log *slog.Logger) *Consumer {
	return &Consumer{rdb: rdb, name: name, m: m, log: log}
}

// Run reads until ctx ends, and returns only after the message in hand is done.
//
// Recovery is the first thing each turn does, and it runs in this loop rather
// than in a goroutine of its own: a separate claimer would have to coordinate
// with the read so the same entry is not processed twice, and that coordination
// is a mutex protecting what one goroutine settles by construction (decisão
// 78).
//
// Neither Redis being unreachable nor a claim that fails stops the loop. There
// is nothing to recover to: the next turn tries again, and the sweep on the
// other side keeps the facts coming.
func (c *Consumer) Run(ctx context.Context, h Handler) error {
	cursor := "0-0"
	for ctx.Err() == nil {
		claimed, next, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   Key,
			Group:    Group,
			Consumer: c.name,
			MinIdle:  claimMinIdle,
			Start:    cursor,
			Count:    readCount,
		}).Result()
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			c.log.Error("claim idle entries", "error", err)
			c.pause(ctx)
			continue
		}
		cursor = next
		if len(claimed) > 0 {
			c.m.Claimed.Add(float64(len(claimed)))
			c.process(ctx, claimed, h)
			continue
		}

		streams, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    Group,
			Consumer: c.name,
			Streams:  []string{Key, ">"},
			Count:    readCount,
			Block:    readBlock,
		}).Result()
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, redis.Nil):
			// The block expired with nothing to read, which is what a healthy
			// cell looks like for minutes at a time.
		case err != nil:
			c.log.Error("read the group", "error", err)
			c.pause(ctx)
		default:
			for _, s := range streams {
				c.process(ctx, s.Messages, h)
			}
		}
	}
	return nil
}

// process handles the entries one by one, in order, stopping between messages
// once the process has been asked to end.
//
// The handler gets a context detached from the shutdown so the message in hand
// finishes instead of failing halfway: an UPDATE cancelled mid-flight would
// leave the entry pending for no reason at all.
func (c *Consumer) process(ctx context.Context, msgs []redis.XMessage, h Handler) {
	for _, msg := range msgs {
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), messageGrace)
		c.handle(work, msg, h)
		cancel()

		if ctx.Err() != nil {
			return
		}
	}
}

func (c *Consumer) handle(ctx context.Context, msg redis.XMessage, h Handler) {
	e, ok := parse(msg)
	if !ok {
		c.bury(ctx, msg)
		return
	}

	if err := h(ctx, e); err != nil {
		// No XACK: the entry stays in the pending list and the next XAUTOCLAIM
		// picks it up. This is the one path that recycles, and it is the right
		// one — the database not answering is exactly the failure worth
		// retrying (decisão 78).
		c.log.Error("close auction", "auction_id", e.AuctionID, "entry", msg.ID, "error", err)
		return
	}

	// Every terminal outcome acknowledges, including the cold ones: asking
	// Redis to redeliver a message that has nothing left to do forever is not
	// durability (decisão 75). Nothing is logged here — a thousand auctions
	// expiring together cannot become a thousand log lines.
	if err := c.rdb.XAck(ctx, Key, Group, msg.ID).Err(); err != nil {
		c.log.Error("acknowledge entry", "entry", msg.ID, "error", err)
	}
}

// bury moves an entry nobody can parse to the dead letter stream.
//
// The dead letter is for malformed payloads only, and not for the n-th
// delivery: in this system the only permanent failures are a payload that is
// not a uuid, caught on the first read, and an auction that is gone, which is a
// terminal outcome that acknowledges. Everything else is the database not
// answering (decisão 78).
func (c *Consumer) bury(ctx context.Context, msg redis.XMessage) {
	values := map[string]any{"entry": msg.ID}
	for k, v := range msg.Values {
		values[k] = v
	}
	if err := c.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: DeadKey,
		MaxLen: maxLen,
		Approx: true,
		Values: values,
	}).Err(); err != nil {
		// Not acknowledged: the dead letter is the only place this entry may
		// end, and losing it silently would erase the evidence of whoever
		// published it.
		c.log.Error("bury malformed entry", "entry", msg.ID, "error", err)
		return
	}
	if err := c.rdb.XAck(ctx, Key, Group, msg.ID).Err(); err != nil {
		c.log.Error("acknowledge buried entry", "entry", msg.ID, "error", err)
		return
	}
	c.m.Dead.Inc()
	c.log.Warn("malformed entry sent to the dead letter stream",
		"entry", msg.ID, "values", msg.Values)
}

func (c *Consumer) pause(ctx context.Context) {
	timer := time.NewTimer(retryPause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func parse(msg redis.XMessage) (Expired, bool) {
	raw, ok := msg.Values[field].(string)
	if !ok {
		return Expired{}, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return Expired{}, false
	}
	return Expired{AuctionID: id}, true
}
