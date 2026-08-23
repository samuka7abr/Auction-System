package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// The absence of chaos.json is the matrix, and it has to stay silent: every one
// of the 36 cells of etapa 5 runs without one.
func TestChaosAbsentIsANormalCell(t *testing.T) {
	got, err := readChaos(t.TempDir())
	if err != nil {
		t.Fatalf("readChaos = %v, want no error", err)
	}
	if got != nil {
		t.Fatalf("readChaos = %+v, want nil", got)
	}
}

func TestChaosLandedIsRead(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "chaos.json"), `{
		"scenario": "closerd-kill", "target": "closerd", "strategy": "optimistic",
		"landed": true,
		"steps": [{"at": "2026-08-23T18:04:35Z", "action": "kill", "target": "closerd"},
		          {"at": "2026-08-23T18:05:15Z", "action": "start", "target": "closerd"}]}`)

	got, err := readChaos(dir)
	if err != nil {
		t.Fatalf("readChaos = %v, want no error", err)
	}
	if !got.Landed || got.Scenario != "closerd-kill" || len(got.Steps) != 2 {
		t.Fatalf("readChaos = %+v", got)
	}
	if line := got.line(); !strings.Contains(line, "closerd-kill") || !strings.Contains(line, "aterrissou") {
		t.Errorf("line = %q", line)
	}
}

// A chaos.json that exists and does not decode is exit 2, by the same rule as
// client.json: a broken artefact gets no verdict, in either direction.
func TestMalformedChaosIsNotVerifiable(t *testing.T) {
	cases := map[string]string{
		"truncated":       `{"scenario": "redis-pause", "landed": tru`,
		"landed not bool": `{"scenario": "redis-pause", "landed": "yes"}`,
		"no scenario":     `{"landed": true}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "client.json"), completeClientJSON)
			write(t, filepath.Join(dir, "env.json"), `{"generator": {"cpuPctPeak": 10, "saturated": false}}`)
			write(t, filepath.Join(dir, "chaos.json"), body)

			if code := execute("cell", dir, false, io.Discard); code != exitUnverifiable {
				t.Errorf("exit = %d, want %d", code, exitUnverifiable)
			}
		})
	}
}

// Decisão 84: an injection that never landed is the worst outcome a chaos cell
// can produce, because it is the one that looks like proof. The two verdicts are
// opposite for the same numbers.
func TestLandedDecidesTheCell(t *testing.T) {
	cases := []struct {
		name  string
		chaos *chaosReport
		want  verdict
	}{
		{"landed", &chaosReport{Scenario: "closerd-kill", Landed: true}, verdictOK},
		{"did not land", &chaosReport{Scenario: "closerd-kill", Landed: false}, verdictFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkCellValidity(baseClient(), calmGenerator(), tc.chaos)
			if got.Verdict != tc.want {
				t.Errorf("verdict = %s (%s), want %s", got.Verdict, got.Detail, tc.want)
			}
		})
	}
}

const completeClientJSON = `{"run":"cell","accepted":1,"conflict":0,"outbid":0,"closed":0,
	"invalid":0,"error":0,"exhausted":0,"attempts":1,"maxSeqSeen":1,
	"replayed":1,"inFlight":1,"duplicatesSelected":1,"duplicatesInjected":1,"transportRetries":0}`
