package shard

// hydrateQuery is the one round-trip a shard pays per auction, the first time
// it sees it. clock_timestamp() is what decisão 50's offset is measured
// against — asking Postgres for the time on every bid is exactly the round-trip
// this engine exists to not pay.
const hydrateQuery = `
SELECT version, highest_bid_cents, min_increment_cents, status, ends_at, clock_timestamp()
  FROM auctions
 WHERE id = $1`

// commitBatch is the one statement a shard runs per batch: an INSERT of every
// accepted bid plus an UPDATE of every auction that batch touched, in one
// round-trip regardless of batch size (decisão 53).
//
// created_at is bound explicitly, at the offset-corrected instant of the
// decision, never DEFAULT now(): the commit lands after the decision, so
// now() at insert time would carry the commit's timestamp instead of the
// decision's, and a bid accepted a moment before ends_at could be written a
// moment after it (decisão 51).
//
// There is no version guard on the UPDATE. In memory, this shard's map is the
// only writer of the auction, so the state it computed is already correct —
// guarding it here would only let a divergence fail silently for that one
// auction while the INSERT of the same batch committed anyway, a partial write
// worse than the problem. What catches divergence is the schema: if this
// shard's memory ever disagreed with the database, the seq it computed already
// belongs to another row, and UNIQUE (auction_id, seq) aborts the whole
// statement — no code, no partial write (decisão 53). For the same reason the
// UPDATE's row count is never checked.
const commitBatch = `
WITH ins AS (
    INSERT INTO bids (id, auction_id, user_id, amount_cents, seq, idempotency_key, created_at)
    SELECT id, auction_id, user_id, amount_cents, seq, NULLIF(key, ''), created_at
      FROM unnest($1::uuid[], $2::uuid[], $3::uuid[], $4::bigint[],
                  $5::bigint[], $6::text[], $7::timestamptz[])
        AS t(id, auction_id, user_id, amount_cents, seq, key, created_at)
)
UPDATE auctions a
   SET highest_bid_cents = v.amount_cents,
       highest_bidder    = v.bidder,
       version           = v.version
  FROM unnest($8::uuid[], $9::bigint[], $10::uuid[], $11::bigint[])
    AS v(id, amount_cents, bidder, version)
 WHERE a.id = v.id`
