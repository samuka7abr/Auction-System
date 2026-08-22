package shard

import (
	"testing"

	"github.com/google/uuid"
)

// shardFor is unexported, so this file lives inside the package: routing is
// pure and needs no database, which is the whole point of testing it apart
// from shard_test.go (RF08).
func TestShardForIsDeterministic(t *testing.T) {
	ids := make([]uuid.UUID, 200)
	for i := range ids {
		ids[i] = uuid.New()
	}

	for _, id := range ids {
		first := shardFor(id, numShards)
		for range 10 {
			if got := shardFor(id, numShards); got != first {
				t.Fatalf("shardFor(%s) = %d, then %d: not deterministic", id, first, got)
			}
		}
	}
}

func TestShardForStaysInRange(t *testing.T) {
	for range 1000 {
		got := shardFor(uuid.New(), numShards)
		if got < 0 || got >= numShards {
			t.Fatalf("shardFor returned %d, want [0, %d)", got, numShards)
		}
	}
}

// The distribution only has to be reasonable, not exact: this guards against a
// routing bug that sends everything to one shard, not against normal skew in a
// random sample.
func TestShardForDistributesOverASample(t *testing.T) {
	const n = 10_000

	counts := make([]int, numShards)
	for range n {
		counts[shardFor(uuid.New(), numShards)]++
	}

	want := n / numShards
	for shard, count := range counts {
		low, high := want/2, want*3/2
		if count < low || count > high {
			t.Errorf("shard %d got %d bids, want roughly %d (between %d and %d)",
				shard, count, want, low, high)
		}
	}
}
