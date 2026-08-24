package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// matrix is the authority. matrix.md is this same content as a table, and the
// jq one-liner in the spec's budget section is the proof that the table is
// derivable from here and not the other way round (decisão 108).
type matrix struct {
	Matrix      string          `json:"matrix"`
	GeneratedAt string          `json:"generatedAt"`
	Git         gitStamp        `json:"git"`
	PoolSize    int64           `json:"poolSize"`
	Host        json.RawMessage `json:"host"`
	Limits      json.RawMessage `json:"limits"`
	Cells       []row           `json:"cells"`
	Control     *control        `json:"control"`
	Warnings    []string        `json:"warnings"`
	Publishable bool            `json:"publishable"`

	// Not published: it is the exit code, not a column.
	Failures int `json:"-"`
}

type gitStamp struct {
	Commit string `json:"commit"`
	Dirty  bool   `json:"dirty"`
}

type row struct {
	Order    int    `json:"order"`
	Run      string `json:"run"`
	Strategy string `json:"strategy"`
	Auctions int64  `json:"auctions"`
	Scenario string `json:"scenario"`
	Policy   string `json:"policy"`
	PeakVUs  int    `json:"peakVUs"`

	DurationMs        float64 `json:"durationMs"`
	Accepted          int64   `json:"accepted"`
	AcceptedPerSecond float64 `json:"acceptedPerSecond"`
	ConflictPerSecond float64 `json:"conflictPerSecond"`
	ConfirmP95Ms      float64 `json:"confirmP95Ms"`
	AttemptsPerAccept float64 `json:"attemptsPerAccept"`
	ExhaustedRate     float64 `json:"exhaustedRate"`

	Checker  checkerStamp `json:"checker"`
	Warnings []string     `json:"warnings"`
}

type checkerStamp struct {
	Warnings int `json:"warnings"`
	Failures int `json:"failures"`
}

type control struct {
	Cell              int       `json:"cell"`
	Repeat            int       `json:"repeat"`
	AcceptedPerSecond []float64 `json:"acceptedPerSecond"`
	Divergence        float64   `json:"divergence"`
	Verdict           string    `json:"verdict"`
}

// ---------------------------------------------------------------- as linhas --

// rows computes the 36 published rows. Cell 37 is not a row: it is the control,
// and it appears in the footer instead.
func (r *reader) rows(p []planned, cells map[string]*loaded, m *matrix) []row {
	out := make([]row, 0, len(p)-1)
	for _, want := range p {
		c, ok := cells[want.Name]
		if !ok {
			continue
		}
		if c.checker.Failures != nil {
			m.Failures += *c.checker.Failures
		}
		if want.Order == 37 {
			continue
		}
		line, ok := r.line(want, c)
		if !ok {
			continue
		}
		out = append(out, line)
	}
	return out
}

// line is RF10. Every formula here is a decision written down somewhere, and
// three of them are decisions about which number NOT to use.
func (r *reader) line(want planned, c *loaded) (row, bool) {
	if c.client.DurationMs == nil || *c.client.DurationMs <= 0 ||
		c.client.Accepted == nil || *c.client.Accepted == 0 ||
		c.client.Conflict == nil || c.client.Attempts == nil || c.client.Exhausted == nil ||
		c.client.ConfirmLatencyMs == nil || c.client.ConfirmLatencyMs.P95 == nil {
		return row{}, false // already refused above; a partial row is not a row
	}
	vus, known := peakVUs(want.Scenario)
	if !known {
		r.refuse("R5", want.Name, "cenário desconhecido: %s", want.Scenario)
		return row{}, false
	}

	seconds := *c.client.DurationMs / 1000
	accepted, conflict := float64(*c.client.Accepted), float64(*c.client.Conflict)
	attempts, exhausted := float64(*c.client.Attempts), float64(*c.client.Exhausted)

	line := row{
		Order: want.Order, Run: filepath.Join(m0(r.dir), want.Name),
		Strategy: want.Strategy, Auctions: want.Auctions,
		Scenario: want.Scenario, Policy: want.Policy, PeakVUs: vus,

		DurationMs: round(*c.client.DurationMs, 4),
		Accepted:   *c.client.Accepted,
		// The window is k6's own, and it is the only honest one: env.json's
		// startedAt..finishedAt covers reset, seed, VACUUM, warmup, a second
		// reset and the checker (decisão 97).
		AcceptedPerSecond: round(accepted/seconds, 4),
		// Only the optimistic engine produces 409. Zero in the other two is
		// information, not a hole.
		ConflictPerSecond: round(conflict/seconds, 4),
		// Client-side, measured by k6. The server side is
		// bid_confirm_duration_seconds, and the distance between the two curves
		// is queueing before the handler — spec 03's subject (decisão 96).
		ConfirmP95Ms: round(*c.client.ConfirmLatencyMs.P95, 4),
		// The global ratio, and deliberately not the clientAttemptsPerAccept
		// trend that already exists: that trend is sampled at the accept, so
		// whoever tried hard and gave up never enters the sample — and it FALLS
		// as contention rises, which is the inverse of what this column promises
		// the reader (decisão 98).
		AttemptsPerAccept: round(attempts/accepted, 4),
		// The share of logical bids that gave up, and what the trend hides.
		ExhaustedRate: round(exhausted/(accepted+exhausted), 4),
	}
	if c.checker.Warnings != nil {
		line.Checker.Warnings = *c.checker.Warnings
	}
	if c.checker.Failures != nil {
		line.Checker.Failures = *c.checker.Failures
	}
	line.Warnings = cellWarnings(c, line.ExhaustedRate)
	return line, true
}

// cellWarnings marks the row without failing the matrix. All three are cases
// where the number exists, is legitimate, and may belong to the instrument
// rather than to the system — refusing would throw away good data, omitting
// would publish it without the caveat it needs (decisão 106).
func cellWarnings(c *loaded, exhausted float64) []string {
	warnings := []string{}
	if c.env.Generator != nil && c.env.Generator.Saturated != nil && *c.env.Generator.Saturated {
		// I6 already warns; the matrix carries it to the table, because an
		// aceitos/s measured with k6 at its ceiling may be k6's.
		warnings = append(warnings, "gerador saturado")
	}
	if exhausted > exhaustionWarn {
		// benchmark.md's threshold. Above it MAX_RETRIES is masking the effect —
		// and raising MAX_RETRIES means running all 36 again, because a matrix
		// with two different bidders is not a matrix.
		warnings = append(warnings, fmt.Sprintf("exaustão acima de 20%%: %s", percent(exhausted)))
	}
	return warnings
}

// -------------------------------------------------------------- o controle --

// control is RF11 and decisão 95. The block is published even when OK: a
// control that only shows up when it fails is a control the reader cannot tell
// ran at all.
func (r *reader) control(p []planned, cells map[string]*loaded, m *matrix) {
	first, repeat := cells[p[0].Name], cells[p[36].Name]
	if first == nil || repeat == nil {
		return
	}
	a, aOK := perSecond(first)
	b, bOK := perSecond(repeat)
	if !aOK || !bOK {
		return
	}

	// Relative to the first, which is the one the other 35 cells were measured
	// after: the question is how far the machine moved while they ran.
	divergence := math.Abs(b-a) / a
	block := control{
		Cell: 1, Repeat: 37,
		AcceptedPerSecond: []float64{round(a, 4), round(b, 4)},
		Divergence:        round(divergence, 4),
		Verdict:           "OK",
	}
	switch {
	case divergence > controlRefuse:
		block.Verdict = "RECUSA"
		r.refuse("R9", p[36].Name, "controle divergiu %s (>%s): a distância entre duas curvas do gráfico "+
			"poderia ser inteiramente explicada pela ordem de execução",
			percent(divergence), percent(controlRefuse))
	case divergence >= controlWarn:
		block.Verdict = "AVISO"
		m.Warnings = append(m.Warnings, fmt.Sprintf("controle divergente: %s", percent(divergence)))
	}
	m.Control = &block
}

func perSecond(c *loaded) (float64, bool) {
	if c.client.Accepted == nil || c.client.DurationMs == nil || *c.client.DurationMs <= 0 {
		return 0, false
	}
	if *c.client.Accepted == 0 {
		return 0, false
	}
	return float64(*c.client.Accepted) / (*c.client.DurationMs / 1000), true
}

// stamp copies what is identical across the 37 by construction. R4 is what
// makes "any cell" a safe thing to say here.
func (r *reader) stamp(p []planned, cells map[string]*loaded, m *matrix) {
	for _, want := range p {
		c, ok := cells[want.Name]
		if !ok || c.env.Git == nil || c.env.Cell == nil || c.env.Cell.PoolSize == nil {
			continue
		}
		m.Git = gitStamp{Commit: c.env.Git.Commit, Dirty: c.env.Git.Dirty != nil && *c.env.Git.Dirty}
		m.PoolSize = *c.env.Cell.PoolSize
		m.Host = c.env.Host
		m.Limits = c.env.Limits
		return
	}
}

// ------------------------------------------------------------- a publicação --

func publish(m matrix, out string) error {
	m.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "matrix.json"), append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "matrix.md"), []byte(markdown(m)), 0o644)
}

// The engine names benchmark.md's Resultados table uses. The table is meant to
// be pasted there without editing, and a header that does not match is a table
// somebody has to retype — which is exactly the source of error the matrix.md
// exists to remove (decisão 108).
var engineName = map[string]string{
	"optimistic":  "Otimista",
	"pessimistic": "Pessimista",
	"shard":       "Single-writer",
}

func markdown(m matrix) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Matriz %s\n\n", m.Matrix)
	fmt.Fprintf(&b, "Commit `%s` · árvore %s · pool %d · %s · gerado em %s\n\n",
		short(m.Git.Commit), dirtyWord(m.Git.Dirty), m.PoolSize,
		plural(len(m.Cells), "célula publicada", "células publicadas"), m.GeneratedAt)

	// Exactly the columns of benchmark.md's Resultados table, plus the one it
	// did not foresee: the checker's warnings per cell. The scenario is not a
	// column of its own because "VUs no pico" already carries it — ramp peaks at
	// 500 and last_second_spike at 1000 (RF10).
	b.WriteString("| Estratégia | Leilões | Retry | VUs no pico | Aceitos/s | p95 confirmação | " +
		"Conflitos/s | Tentativas por aceito | Exauridos | Invariantes | Avisos |\n")
	b.WriteString(strings.Repeat("| --- ", 11) + "|\n")
	for _, c := range m.Cells {
		invariants := "OK"
		if c.Checker.Failures > 0 {
			invariants = fmt.Sprintf("FALHA (%d)", c.Checker.Failures)
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %d | %.2f | %.1f | %.2f | %.2f | %s | %s | %s |\n",
			engineName[c.Strategy], c.Auctions, c.Policy, c.PeakVUs,
			c.AcceptedPerSecond, c.ConfirmP95Ms, c.ConflictPerSecond,
			c.AttemptsPerAccept, percent(c.ExhaustedRate), invariants,
			warningCell(c))
	}

	b.WriteString("\n## Controle\n\n")
	if m.Control == nil {
		b.WriteString("Não rodou: a célula 01 ou a 37 não produziu número.\n")
	} else {
		fmt.Fprintf(&b, "A célula 01 repetida por último (37): %.2f vs %.2f aceitos/s · divergência %s · **%s**\n",
			m.Control.AcceptedPerSecond[0], m.Control.AcceptedPerSecond[1],
			percent(m.Control.Divergence), m.Control.Verdict)
	}

	b.WriteString("\n## Avisos\n\n")
	lines := append([]string{}, m.Warnings...)
	for _, c := range m.Cells {
		for _, w := range c.Warnings {
			lines = append(lines, fmt.Sprintf("célula %02d · %s", c.Order, w))
		}
	}
	if len(lines) == 0 {
		b.WriteString("Nenhum.\n")
	}
	for _, l := range lines {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	return b.String()
}

func warningCell(c row) string {
	parts := append([]string{}, c.Warnings...)
	if c.Checker.Warnings > 0 {
		parts = append(parts, plural(c.Checker.Warnings, "aviso do checker", "avisos do checker"))
	}
	if len(parts) == 0 {
		return "—"
	}
	return joinNonEmpty(parts, "; ")
}

func dirtyWord(dirty bool) string {
	if dirty {
		return "SUJA"
	}
	return "limpa"
}

// m0 is the matrix identifier, which is the name of the directory holding the
// cells — the same string the loop used as $MATRIX.
func m0(dir string) string { return filepath.Base(dir) }

func percent(rate float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", rate*100), "0"), ".") + "%"
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
