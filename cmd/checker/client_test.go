package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDurabilityReadsTheThreeWaysTheCountsCanDiffer(t *testing.T) {
	cases := []struct {
		name string
		db   cellTotals
		want verdict
	}{
		// db == client: nothing was confirmed that the database does not have.
		{"equal", cellTotals{Bids: 100, MaxSeq: 100}, verdictOK},
		// Idempotent transport recovery makes a durable response observable, so
		// either direction is now a failed cell.
		{"database ahead", cellTotals{Bids: 101, MaxSeq: 100}, verdictFail},
		// db < client: a lost write, and the failure this harness exists for.
		{"database behind", cellTotals{Bids: 99, MaxSeq: 100}, verdictFail},
		// The watermark catches either direction even when counts agree.
		{"watermark behind", cellTotals{Bids: 100, MaxSeq: 99}, verdictFail},
		{"watermark ahead", cellTotals{Bids: 100, MaxSeq: 101}, verdictFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkDurability(tc.db, baseClient(), nil); got.Verdict != tc.want {
				t.Errorf("verdict = %s (%s), want %s", got.Verdict, got.Detail, tc.want)
			}
		})
	}
}

// The six directions of decisão 85, and the two that do not soften are the
// reason the table exists: killing the process that would have answered
// explains a database ahead of the client, and explains nothing at all about a
// confirmed bid the database does not have.
func TestDurabilityUnderChaosLoosensOnlyTheSafeDirection(t *testing.T) {
	cases := []struct {
		name string
		db   cellTotals
		want verdict
	}{
		{"equal", cellTotals{Bids: 100, MaxSeq: 100}, verdictOK},
		// The 201 was written and the response died with the process.
		{"database ahead", cellTotals{Bids: 101, MaxSeq: 100}, verdictWarn},
		{"watermark ahead", cellTotals{Bids: 100, MaxSeq: 101}, verdictWarn},
		// A confirmed bid that vanished. The headline of the etapa, and it does
		// not move for any injection.
		{"database behind", cellTotals{Bids: 99, MaxSeq: 100}, verdictFail},
		{"watermark behind", cellTotals{Bids: 100, MaxSeq: 99}, verdictFail},
		// One direction on each half: the harsher one has to survive.
		{"ahead on count, behind on watermark", cellTotals{Bids: 101, MaxSeq: 99}, verdictFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chaos := &chaosReport{Scenario: "auctiond-kill", Target: "auctiond", Strategy: "shard", Landed: true}
			if got := checkDurability(tc.db, baseClient(), chaos); got.Verdict != tc.want {
				t.Errorf("verdict = %s (%s), want %s", got.Verdict, got.Detail, tc.want)
			}
		})
	}
}

func TestCellValidity(t *testing.T) {
	cases := []struct {
		name   string
		client func(*clientReport)
		env    envReport
		want   verdict
	}{
		{"clean cell", func(*clientReport) {}, calmGenerator(), verdictOK},
		// outcome="invalid" above zero means the k6 sent a bid without
		// expectedVersion: the cell is measuring a misconfiguration.
		{"k6 misconfigured", func(c *clientReport) { c.Invalid = i64(1) }, calmGenerator(), verdictFail},
		// The threshold is a boundary, so both sides of it are pinned.
		{"errors at one percent", func(c *clientReport) { c.Error = i64(10); c.Attempts = i64(1000) }, calmGenerator(), verdictFail},
		{"errors just under", func(c *clientReport) { c.Error = i64(9); c.Attempts = i64(1000) }, calmGenerator(), verdictOK},
		{"no attempts at all", func(c *clientReport) { c.Attempts = i64(0) }, calmGenerator(), verdictFail},
		{"duplicates not selected", func(c *clientReport) { c.DuplicatesSelected = i64(0) }, calmGenerator(), verdictFail},
		{"duplicates not injected", func(c *clientReport) { c.DuplicatesInjected = i64(0) }, calmGenerator(), verdictFail},
		{"too many duplicates", func(c *clientReport) { c.DuplicatesInjected = i64(11) }, calmGenerator(), verdictFail},
		{"no replay", func(c *clientReport) { c.Replayed = i64(0) }, calmGenerator(), verdictFail},
		{"no in-flight response", func(c *clientReport) { c.InFlight = i64(0) }, calmGenerator(), verdictWarn},
		{"negative counter", func(c *clientReport) { c.TransportRetries = i64(-1) }, calmGenerator(), verdictFail},
		// A saturated generator warns rather than fails: discarding the cell is
		// the reader's call, but never a silent one.
		{"generator saturated", func(*clientReport) {}, saturatedGenerator(), verdictWarn},
		{"auctions closed mid-cell", func(c *clientReport) { c.Closed = i64(3) }, calmGenerator(), verdictWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseClient()
			tc.client(&c)
			if got := checkCellValidity(c, tc.env, nil); got.Verdict != tc.want {
				t.Errorf("verdict = %s (%s), want %s", got.Verdict, got.Detail, tc.want)
			}
		})
	}
}

// Under chaos the error rate is the injection, and the four checks that are
// about the generator being configured right are not: those keep failing,
// because the generator is not what is being killed.
func TestCellValidityUnderChaos(t *testing.T) {
	landed := &chaosReport{Scenario: "redis-pause", Target: "redis", Strategy: "optimistic", Landed: true}
	cases := []struct {
		name   string
		client func(*clientReport)
		want   verdict
	}{
		{"errors are the injection", func(c *clientReport) { c.Error = i64(120); c.Attempts = i64(1000) }, verdictWarn},
		{"auctions closing is the point", func(c *clientReport) { c.Closed = i64(3) }, verdictOK},
		// The generator is not the target of any of the four injections.
		{"k6 misconfigured", func(c *clientReport) { c.Invalid = i64(1) }, verdictFail},
		{"negative counter", func(c *clientReport) { c.TransportRetries = i64(-1) }, verdictFail},
		{"duplicates not selected", func(c *clientReport) { c.DuplicatesSelected = i64(0) }, verdictFail},
		{"no replay", func(c *clientReport) { c.Replayed = i64(0) }, verdictFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseClient()
			tc.client(&c)
			if got := checkCellValidity(c, calmGenerator(), landed); got.Verdict != tc.want {
				t.Errorf("verdict = %s (%s), want %s", got.Verdict, got.Detail, tc.want)
			}
		})
	}
}

func TestIdempotencyFieldsAreRequired(t *testing.T) {
	fields := []string{"replayed", "inFlight", "duplicatesSelected", "duplicatesInjected", "transportRetries"}
	complete := map[string]any{
		"accepted": 1, "conflict": 0, "outbid": 0, "closed": 0,
		"invalid": 0, "error": 0, "exhausted": 0, "attempts": 1, "maxSeqSeen": 1,
		"replayed": 1, "inFlight": 1, "duplicatesSelected": 1,
		"duplicatesInjected": 1, "transportRetries": 0,
	}

	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			doc := make(map[string]any, len(complete))
			for key, value := range complete {
				doc[key] = value
			}
			delete(doc, field)
			body, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("marshal fixture: %v", err)
			}
			dir := t.TempDir()
			write(t, filepath.Join(dir, "client.json"), string(body))

			_, err = readClient(dir)
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("readClient error = %v, want missing %s", err, field)
			}
		})
	}
}

// A client.json that cannot be read is exit 2 and never exit 0: the alternative
// is a durability invariant passing green against a zero it invented.
func TestUnreadableArtefactsAreNotVerifiable(t *testing.T) {
	cases := map[string]string{
		"absent":        "",
		"truncated":     `{"run": "t", "accepted": 100`,
		"missing field": `{"run": "t", "accepted": 100, "conflict": 0}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if body != "" {
				write(t, filepath.Join(dir, "client.json"), body)
				write(t, filepath.Join(dir, "env.json"), `{"generator": {"cpuPctPeak": 10, "saturated": false}}`)
			}
			if code := execute("cell", dir, false, io.Discard); code != exitUnverifiable {
				t.Errorf("exit = %d, want %d", code, exitUnverifiable)
			}
		})
	}
}

func TestEnvWithoutGeneratorIsNotVerifiable(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "client.json"), `{"accepted":1,"conflict":0,"outbid":0,"closed":0,
		"invalid":0,"error":0,"exhausted":0,"attempts":1,"maxSeqSeen":1,
		"replayed":1,"inFlight":1,"duplicatesSelected":1,"duplicatesInjected":1,"transportRetries":0}`)
	write(t, filepath.Join(dir, "env.json"), `{"run": "cell"}`)

	if code := execute("cell", dir, false, io.Discard); code != exitUnverifiable {
		t.Errorf("exit = %d, want %d", code, exitUnverifiable)
	}
}

func baseClient() clientReport {
	return clientReport{
		Run: "t", Strategy: "optimistic", Auctions: 1, Policy: "immediate", Scenario: "smoke",
		Accepted: i64(100), Conflict: i64(400), Outbid: i64(10), Closed: i64(0),
		Invalid: i64(0), Error: i64(0), Exhausted: i64(2), Attempts: i64(600), MaxSeqSeen: i64(100),
		Replayed: i64(5), InFlight: i64(3), DuplicatesSelected: i64(10),
		DuplicatesInjected: i64(10), TransportRetries: i64(0),
	}
}

func calmGenerator() envReport {
	return envReport{Generator: &generatorReport{CPUPctPeak: f64(61), Saturated: boolOf(false)}}
}

func saturatedGenerator() envReport {
	return envReport{Generator: &generatorReport{CPUPctPeak: f64(97), Saturated: boolOf(true)}}
}

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }
func boolOf(v bool) *bool    { return &v }

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
