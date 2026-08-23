package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// chaosStep is one thing the injector did, and the moment it did it. The
// checker never reads it — it exists so that the report the reader opens carries
// the timeline next to the verdicts instead of in a separate log.
type chaosStep struct {
	At     string `json:"at"`
	Action string `json:"action"`
	Target string `json:"target"`
}

// chaosReport is what the injector left behind. Absent means a normal cell;
// malformed means exit 2, by the same rule as client.json — a file that exists
// and does not decode is a broken artefact, and a cell without an intact
// artefact gets no verdict.
//
// Only Scenario and Landed change a verdict (decisões 84 and 85). Evidence is
// deliberately not modelled: it has a different shape per scenario, and forcing
// four failures that look nothing alike into one schema would produce null
// columns nobody can read.
type chaosReport struct {
	Scenario string      `json:"scenario"`
	Target   string      `json:"target"`
	Strategy string      `json:"strategy"`
	Landed   bool        `json:"landed"`
	Steps    []chaosStep `json:"steps"`
}

// readChaos returns (nil, nil) when the cell has no chaos.json, which is every
// cell of the matrix. It is the one artefact of this harness whose absence is
// not an error.
func readChaos(dir string) (*chaosReport, error) {
	path := filepath.Join(dir, "chaos.json")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read chaos.json: %w", err)
	}
	var c chaosReport
	if err := readFile(path, &c); err != nil {
		return nil, err
	}
	// A report that does not name its scenario cannot be told from a stray file,
	// and "which failure was injected" is the first thing every verdict below
	// depends on.
	if c.Scenario == "" {
		return nil, fmt.Errorf("chaos.json is missing scenario")
	}
	return &c, nil
}

// line is what render prints above the verdicts, so that a reader never mistakes
// a cell with a process killed in it for a healthy one.
func (c *chaosReport) line() string {
	landed := "NÃO ATERRISSOU"
	if c.Landed {
		landed = "aterrissou"
	}
	return fmt.Sprintf("caos: %s · alvo %s · estratégia %s · %s · %s",
		c.Scenario, c.Target, c.Strategy, plural(len(c.Steps), "passo", "passos"), landed)
}

// worse keeps the harshest of two verdicts. I5 reaches two independent
// conclusions — one on the counts, one on the watermark — and under chaos one of
// them can soften while the other does not.
func worse(a, b verdict) verdict {
	rank := map[verdict]int{verdictOK: 0, verdictWarn: 1, verdictFail: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
