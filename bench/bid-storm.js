// The aggressive bidder of decisão 5, and the only load generator this project
// has. One script for the three engines: it never branches on strategy.
//
// 409 and 422 are the same event seen through different mechanisms (decisão 9),
// so the pessimistic and shard engines — which never produce a 409 — carry
// exactly the load the optimistic one carries. That is what makes accepted/s
// comparable at all.
import http from 'k6/http';
import { sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import { SharedArray } from 'k6/data';

const RUN = __ENV.RUN || 'dev';
const POLICY = __ENV.RETRY_POLICY || 'immediate';
const SCENARIO = __ENV.SCENARIO || 'smoke';
const STRATEGY = __ENV.BID_STRATEGY || 'optimistic';
const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const RESULTS = __ENV.RESULTS_DIR || 'bench/results';
const WARMUP = __ENV.WARMUP === '1';

// A real bidder gives up on time, not on a count; the two together model that
// (decisão 18). Whichever comes first.
const MAX_RETRIES = 10;
const BID_DEADLINE = 2000;

// The manifest, not a GET per auction: at 1000 auctions the preparation reads
// would be pure noise on top of the measurement.
const auctions = new SharedArray('auctions', () => JSON.parse(open('./auctions.json')));

// k6 counts every status >= 400 as a failure. Without this, each conflict would
// land in http_req_failed and the rate<0.01 threshold would fail the very cell
// the thesis is about, before the tenth request. 404, 400 and 503 stay
// failures, and must: none of the three is an expected answer here.
http.setResponseCallback(http.expectedStatuses(201, 409, 410, 422, 425));

const accepted = new Counter('bids_accepted');
const conflict = new Counter('bids_conflict');
const outbid = new Counter('bids_outbid');
const closed = new Counter('bids_closed');
const invalid = new Counter('bids_invalid');
const errored = new Counter('bids_error');
// Headline of the report, not a footnote: under high contention this IS the
// optimistic engine's collapse made visible.
const exhausted = new Counter('bids_exhausted');
const attempts = new Counter('bids_attempts');
const replayed = new Counter('bids_replayed');
const inFlight = new Counter('bids_in_flight');
const duplicatesSelected = new Counter('duplicates_selected');
const duplicatesInjected = new Counter('duplicates_injected');
const transportRetries = new Counter('transport_retries');

// Distribution rather than a mean (decisão 16), and sampled only on accept:
// mixing in the bidders that gave up would produce a number that is neither
// amplification nor abandonment rate. Those are in bids_exhausted.
//
// client_, because bid_attempts_per_accept is now the server's, observed by the
// idempotency middleware. The two do not measure the same thing and both are
// kept: this one counts the attempts the client MADE, the server's counts the
// ones that ARRIVED, and under 1000 VUs a bidder gives up on BID_DEADLINE while
// the server still holds its request. The gap between the curves is that loss
// (decisão 38). Same name in two places would be the surest way for somebody to
// plot the wrong one.
const clientAttemptsPerAccept = new Trend('client_attempts_per_accept');
// The client twin of bid_confirm_duration_seconds{strategy}. The gap between
// the two curves is queueing ahead of the handler, which is what separates "the
// server is slow" from "the client is in line".
const confirmLatency = new Trend('bid_confirm_latency');
// max is the watermark I5 reads. A Gauge would export the LAST value rather
// than the largest, and handleSummary gets no per-tag breakdown — the number
// would be whatever VU wrote last (emenda 28).
const seqSeen = new Trend('seq_seen');

const SCENARIOS = {
  // Exists to exercise the harness and the checkpoints without waiting out a
  // two-and-a-half minute ramp.
  smoke: { executor: 'constant-vus', vus: 20, duration: '15s' },
  ramp: {
    executor: 'ramping-vus',
    startVUs: 0,
    stages: [
      { duration: '30s', target: 100 },
      { duration: '1m', target: 500 },
      { duration: '30s', target: 0 },
    ],
  },
  last_second_spike: { executor: 'constant-vus', vus: 1000, duration: '15s' },
};

if (!SCENARIOS[SCENARIO]) {
  throw new Error(`SCENARIO=${SCENARIO} is not one of ${Object.keys(SCENARIOS).join(', ')}`);
}

export const options = {
  scenarios: { [SCENARIO]: WARMUP ? shorten(SCENARIOS[SCENARIO]) : SCENARIOS[SCENARIO] },
  // One threshold, and it is about validity rather than performance. A latency
  // threshold would fail the cell for measuring what it was built to measure:
  // the optimistic engine under 1000 VUs is *supposed* to blow past p99 500ms,
  // and that is the graph. Whether the number is acceptable is etapa 6's call,
  // not k6's. Above 1% real errors the measurement broke, not the engine.
  thresholds: WARMUP ? {} : { http_req_failed: ['rate<0.01'] },
  // client.json publishes these three and nothing else, so nothing else is
  // computed. max is what I5 depends on.
  summaryTrendStats: ['avg', 'p(95)', 'max'],
};

// The warmup fills the pool, the per-connection plan cache and the page cache.
// It does not have to be long, and it must not be able to fail anything.
function shorten(scenario) {
  const cut = (d) => `${Math.max(3, Math.round(seconds(d) / 5))}s`;
  const short = Object.assign({}, scenario);
  if (short.duration) short.duration = cut(short.duration);
  if (short.stages) short.stages = short.stages.map((s) => Object.assign({}, s, { duration: cut(s.duration) }));
  return short;
}

function seconds(d) {
  const parsed = /^(\d+)(ms|s|m)$/.exec(d);
  if (!parsed) throw new Error(`unsupported duration ${d}`);
  const n = Number(parsed[1]);
  return parsed[2] === 'm' ? n * 60 : parsed[2] === 'ms' ? n / 1000 : n;
}

// Per VU, never global: two VUs bidding on the same auction have to be able to
// hold stale views of each other, or there is no dispute left to measure.
const known = {};

// Seeded by the manifest, then updated from the body of every response that
// carries state. Starting each logical bid from the manifest would make the
// first attempt after that auction's first bid a guaranteed 409 — a constant
// +1 on bid_attempts_per_accept, an artefact of the harness, and one that would
// show up in the optimistic engine alone (emenda 26).
function stateOf(auction) {
  let state = known[auction.id];
  if (!state) {
    state = { currentVersion: auction.version, minNextBid: auction.minNextBid };
    known[auction.id] = state;
  }
  return state;
}

function absorb(state, body) {
  if (body && typeof body.currentVersion === 'number' && body.currentVersion >= state.currentVersion) {
    state.currentVersion = body.currentVersion;
    state.minNextBid = body.minNextBid;
  }
}

function backoff(attempt) {
  if (POLICY === 'immediate') return 0;
  return Math.random() * Math.min(200, 5 * 2 ** attempt); // full jitter
}

// Deterministic, so that highest_bidder in the database is traceable back to a
// VU at the cost of no per-request record.
function userID() {
  return `00000000-0000-4000-8000-${String(__VU).padStart(12, '0')}`;
}

// Drawn once, in the init context, and it is not decoration: __VU and __ITER
// both restart at zero between the warmup and the measured run, so without it
// the measured run would collect the warmup's stored answers as replays — a
// whole cell of 201s that wrote no row, and nothing in the report saying so.
const NONCE = hex(18);

function hex(n) {
  let out = '';
  for (let i = 0; i < n; i++) out += Math.floor(Math.random() * 16).toString(16);
  return out;
}

// One key per LOGICAL BID, kept across every retry of it (decisão 31). The
// bidder re-aims after each rejection, so attempt 2 carries a different body
// from attempt 1 by construction: a key per request would either break the
// thread the server needs or bounce the bidder off its own middleware.
//
// Shaped as a v4 UUID because the middleware validates it as it validates
// X-User-Id, and unique by construction: every hex digit of the iteration and
// of the VU is in there, next to the nonce of this k6 process. Four digits hold
// 65535 VUs and the largest scenario of the project runs 1000.
function bidKey() {
  const vu = __VU.toString(16).padStart(4, '0');
  const iter = __ITER.toString(16).padStart(8, '0');
  return `${iter}-${vu}-4${NONCE.slice(0, 3)}-8${NONCE.slice(3, 6)}-${NONCE.slice(6)}`;
}

// One slot in every ten logical bids, offset by VU so a cell does not begin
// with all VUs injecting at once. Consecutive selected slots alternate the two
// demonstrations. Nothing about the response or the engine enters this choice.
function duplicateMode() {
  const logical = __ITER + __VU - 1;
  if (logical % 10 !== 0) return '';
  return Math.floor(logical / 10) % 2 === 0 ? 'concurrent' : 'replay';
}

function isReplay(res) {
  for (const name of Object.keys(res.headers || {})) {
    if (name.toLowerCase() === 'x-idempotency-replayed') {
      return String(res.headers[name]).toLowerCase() === 'true';
    }
  }
  return false;
}

// A batch has no meaningful "original" response, so reduce by semantics. All
// observed statuses keep their own counters, but any 201 wins the control-flow
// decision. Without a 201, an unexpected terminal status wins over retryable
// rejection, 425 or transport ambiguity.
function reduceResponses(responses, state) {
  let acceptedResponse = null;
  let terminal = false;
  let transports = 0;

  for (const res of responses) {
    if (res.status === 201) {
      if (isReplay(res)) replayed.add(1);
      if (acceptedResponse === null) acceptedResponse = res;
      continue;
    }

    if (res.status === 409 || res.status === 422) {
      (res.status === 409 ? conflict : outbid).add(1);
      absorb(state, res.json());
      continue;
    }

    if (res.status === 425) {
      inFlight.add(1);
      continue;
    }

    if (res.status === 0) {
      errored.add(1);
      transports++;
      continue;
    }

    if (res.status === 410) {
      closed.add(1);
      absorb(state, res.json());
    } else if (res.status === 400) {
      // outcome="invalid" above zero denounces a k6 sending a bid without
      // expectedVersion (decisão 21). I6 fails the cell on it.
      invalid.add(1);
    } else {
      errored.add(1);
    }
    terminal = true;
  }

  if (acceptedResponse !== null) return { acceptedResponse };
  if (transports > 0) transportRetries.add(transports);
  return { terminal };
}

function demonstrateReplay(url, body, params, originalBody) {
  duplicatesInjected.add(1);
  const res = http.post(url, body, params);
  const replay = res.status === 201 && isReplay(res);

  if (replay) replayed.add(1);
  if (res.status === 425) inFlight.add(1);
  if (!replay || res.body !== originalBody) errored.add(1);
}

export default function () {
  const auction = auctions[Math.floor(Math.random() * auctions.length)];
  const state = stateOf(auction);
  const url = `${BASE_URL}/auctions/${auction.id}/bids`;
  const params = {
    headers: {
      'Content-Type': 'application/json',
      'X-User-Id': userID(),
      // Sent on every attempt and on the one optional duplicate of this logical
      // bid. A retry may re-aim its body; the key still never changes.
      'X-Idempotency-Key': bidKey(),
    },
  };
  const mode = duplicateMode();
  if (mode !== '') duplicatesSelected.add(1);
  const deadline = Date.now() + BID_DEADLINE;

  for (let attempt = 0; attempt < MAX_RETRIES && Date.now() < deadline; attempt++) {
    attempts.add(1);
    // The auction owns the increment (decisão 17); the client never invents a
    // value, or two VUs would compete under different rules.
    const body = JSON.stringify({ amountCents: state.minNextBid, expectedVersion: state.currentVersion });
    let responses;
    if (mode === 'concurrent' && attempt === 0) {
      duplicatesInjected.add(1);
      responses = http.batch([
        ['POST', url, body, params],
        ['POST', url, body, params],
      ]);
    } else {
      responses = [http.post(url, body, params)];
    }

    const result = reduceResponses(responses, state);
    if (result.acceptedResponse) {
      const res = result.acceptedResponse;
      const accept = res.json();
      absorb(state, accept);
      accepted.add(1);
      seqSeen.add(accept.seq);
      clientAttemptsPerAccept.add(attempt + 1);
      confirmLatency.add(res.timings.duration);
      if (mode === 'replay') demonstrateReplay(url, body, params, res.body);
      return;
    }

    if (result.terminal) return;
    sleep(backoff(attempt) / 1000);
  }
  exhausted.add(1);
}

export function handleSummary(data) {
  // The warmup is discarded, and so is its report: leaving one behind would let
  // the checker verify the wrong run if the measured k6 then failed.
  if (WARMUP) return { stdout: 'warmup discarded\n' };

  const client = {
    run: RUN,
    strategy: STRATEGY,
    auctions: auctions.length,
    policy: POLICY,
    scenario: SCENARIO,
    accepted: count(data, 'bids_accepted'),
    conflict: count(data, 'bids_conflict'),
    outbid: count(data, 'bids_outbid'),
    closed: count(data, 'bids_closed'),
    invalid: count(data, 'bids_invalid'),
    error: count(data, 'bids_error'),
    exhausted: count(data, 'bids_exhausted'),
    attempts: count(data, 'bids_attempts'),
    replayed: count(data, 'bids_replayed'),
    inFlight: count(data, 'bids_in_flight'),
    duplicatesSelected: count(data, 'duplicates_selected'),
    duplicatesInjected: count(data, 'duplicates_injected'),
    transportRetries: count(data, 'transport_retries'),
    maxSeqSeen: trend(data, 'seq_seen').max,
    durationMs: duration(data),
    confirmLatencyMs: trend(data, 'bid_confirm_latency'),
    clientAttemptsPerAccept: trend(data, 'client_attempts_per_accept'),
  };

  const dir = `${RESULTS}/${RUN}`;
  return {
    stdout: `${JSON.stringify(client, null, 2)}\n`,
    // The raw record. Kept, and never read by the checker: the shape of `data`
    // is internal to k6 and moves between versions.
    [`${dir}/summary.json`]: JSON.stringify(data),
    [`${dir}/client.json`]: JSON.stringify(client, null, 2),
  };
}

// A Counter that never took a sample is absent from data.metrics, and for a
// counter absence has exactly one reading: zero. A value that is present and
// not a number is a different animal and throws — a summary shape that changed
// under a k6 upgrade has to break here, loudly, rather than three steps later
// as a durability invariant passing green against a silent zero.
function count(data, name) {
  const metric = data.metrics[name];
  if (metric === undefined) return 0;
  const value = metric.values && metric.values.count;
  if (typeof value !== 'number') throw new Error(`metric ${name} has no numeric count`);
  return value;
}

// A Trend has no such reading. seq_seen missing means no bid was ever accepted,
// and reporting maxSeqSeen: 0 would let I5 pass against nothing at all.
function trend(data, name) {
  const metric = data.metrics[name];
  if (metric === undefined) throw new Error(`metric ${name} is missing from the k6 summary`);
  const values = metric.values || {};
  const out = { avg: values.avg, p95: values['p(95)'], max: values.max };
  for (const key of Object.keys(out)) {
    if (typeof out[key] !== 'number') throw new Error(`metric ${name} has no numeric ${key}`);
  }
  return out;
}

// The window the rates of etapa 5 are computed over, and the only honest one:
// env.json's startedAt..finishedAt covers reset, seed, vacuum, warmup, a second
// reset and the checker, which for a 15s spike is four times the load itself.
//
// It throws instead of returning zero, by the same rule as count and trend: a
// summary shape that moved under a k6 upgrade has to break here and loudly, and
// not three steps later as a green rate computed over nothing (decisão 97).
function duration(data) {
  const ms = data.state && data.state.testRunDurationMs;
  if (typeof ms !== 'number' || !(ms > 0)) throw new Error('k6 summary has no testRunDurationMs');
  return ms;
}
