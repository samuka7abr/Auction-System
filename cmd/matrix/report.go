package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
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

	DurationMs float64 `json:"durationMs"`
	// A count, and it stays a count on an interrupted cell: the accepts
	// happened, and what does not exist is the rate.
	Accepted int64 `json:"accepted"`
	// The three per-second numbers, and pointers because on a cell that did not
	// converge they are ABSENT rather than zero. Publishing 4238 ÷ 2602s = 1.63
	// would draw a point on the graph saying "the pessimist delivers 1.63 bids
	// per second", when what was measured is "it accepted 4238 and then took
	// forty minutes to drain" — a different sentence, and a truer one.
	AcceptedPerSecond *float64 `json:"acceptedPerSecond"`
	ConflictPerSecond *float64 `json:"conflictPerSecond"`
	AttemptsPerAccept *float64 `json:"attemptsPerAccept"`
	// Distributions, not rates: a shorter window samples them, it does not
	// dilute them, so they are published for an interrupted cell too.
	ConfirmP95Ms  float64 `json:"confirmP95Ms"`
	ExhaustedRate float64 `json:"exhaustedRate"`
	// The harness stopped watching before the load ended (RF04).
	Interrupted bool `json:"interrupted"`

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

		DurationMs:  round(*c.client.DurationMs, 4),
		Accepted:    *c.client.Accepted,
		Interrupted: interrupted(c),
		// The window is k6's own, and it is the only honest one: env.json's
		// startedAt..finishedAt covers reset, seed, VACUUM, warmup, a second
		// reset and the checker (decisão 97).
		AcceptedPerSecond: rate(accepted/seconds, c),
		// Only the optimistic engine produces 409. Zero in the other two is
		// information, not a hole.
		ConflictPerSecond: rate(conflict/seconds, c),
		// Client-side, measured by k6. The server side is
		// bid_confirm_duration_seconds, and the distance between the two curves
		// is queueing before the handler — spec 03's subject (decisão 96).
		ConfirmP95Ms: round(*c.client.ConfirmLatencyMs.P95, 4),
		// The global ratio, and deliberately not the clientAttemptsPerAccept
		// trend that already exists: that trend is sampled at the accept, so
		// whoever tried hard and gave up never enters the sample — and it FALLS
		// as contention rises, which is the inverse of what this column promises
		// the reader (decisão 98).
		AttemptsPerAccept: rate(attempts/accepted, c),
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
	if line.Interrupted {
		// The seconds come from the client's own window and not from the
		// CELL_BUDGET the loop was given: what belongs in the table is how long
		// the cell was actually watched.
		line.Warnings = append(line.Warnings,
			fmt.Sprintf("interrompida em %.0fs: não convergiu dentro do orçamento", seconds))
	}
	return line, true
}

// rate is where the whole of spec 02 fits: a cell that did not converge has no
// per-second number, and the absence is published as absence. Refusing the cell
// would turn the strongest finding of the project into the reason there is no
// result; publishing the diluted number would put a false point on the graph.
func rate(v float64, c *loaded) *float64 {
	if interrupted(c) {
		return nil
	}
	v = round(v, 4)
	return &v
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
	// The last cell of the plan and not the 37th of a list: the slice has ten
	// cells and the control is still the last of them.
	firstName, repeatName := p[0].Name, p[len(p)-1].Name
	first, repeat := cells[firstName], cells[repeatName]
	if first == nil || repeat == nil {
		return
	}
	// The one place an interrupted cell does refuse the matrix. The control
	// measures how far the machine moved between two runs of the same cell, and
	// a distance measured over two different windows is not that distance
	// (RF04).
	truncated := false
	for _, side := range []struct {
		name string
		c    *loaded
	}{{firstName, first}, {repeatName, repeat}} {
		if interrupted(side.c) {
			r.refuse("R9", side.name, "célula do controle interrompida: um controle medido "+
				"sobre uma janela truncada não é um controle")
			truncated = true
		}
	}
	if truncated {
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
		r.refuse("R9", repeatName, "controle divergiu %s (>%s): a distância entre duas curvas do gráfico "+
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
		switch {
		case c.Checker.Failures > 0:
			invariants = fmt.Sprintf("FALHA (%d)", c.Checker.Failures)
		case c.Interrupted:
			// Not a failure and not an OK either: the eight invariants held over
			// a window that ended before the load did.
			invariants = "não convergiu"
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %d | %s | %.1f | %s | %s | %s | %s | %s |\n",
			engineName[c.Strategy], c.Auctions, c.Policy, c.PeakVUs,
			num(c.AcceptedPerSecond), c.ConfirmP95Ms, num(c.ConflictPerSecond),
			num(c.AttemptsPerAccept), percent(c.ExhaustedRate), invariants,
			warningCell(c))
	}
	b.WriteString(graph(m))

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

// A cell without a rate is an em dash, never a zero: zero is a measurement and
// this is its absence.
func num(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f", *v)
}

// ---------------------------------------------------------------- o gráfico --

// The deliverable benchmark.md asks for, in text: throughput by level of
// contention, one line per strategy, with the crossing point marked.
//
// Text and not SVG because the consumer is etapa 6, which writes markdown, and
// a graph that pastes into benchmark.md today is worth more than an image file
// somebody has to open. It reads the ramp/immediate cells and only those: they
// are the two variables of this graph, and in a full matrix the other three
// quarters of the rows answer two other questions (RF05).
func graph(m matrix) string {
	levels := []int64{1, 10, 1000}
	engines := []string{"optimistic", "pessimistic", "shard"}
	at := map[string]map[int64]*float64{}
	for _, c := range m.Cells {
		if c.Scenario != "ramp" || c.Policy != "immediate" {
			continue
		}
		if at[c.Strategy] == nil {
			at[c.Strategy] = map[int64]*float64{}
		}
		at[c.Strategy][c.Auctions] = c.AcceptedPerSecond
	}

	var b strings.Builder
	b.WriteString("\n## Gráfico principal\n\n```text\naceitos/s por contenção · ramp · immediate\n\n")
	b.WriteString(padRight("", labelWidth))
	for _, n := range levels {
		b.WriteString(padLeft(plural(int(n), "leilão", "leilões"), columnWidth))
	}
	b.WriteString("\n")
	for _, e := range engines {
		b.WriteString(padRight(engineName[e], labelWidth))
		for _, n := range levels {
			b.WriteString(padLeft(num(at[e][n]), columnWidth))
		}
		b.WriteString("\n")
	}
	for _, line := range crossings(engines, levels, at) {
		b.WriteString("\n" + line + "\n")
	}
	b.WriteString("```\n")
	return b.String()
}

// The crossing point is the reason the graph exists: it is where the answer
// stops being "one engine is faster" and becomes "which one depends on the
// contention". A pair whose order never flips has no crossing, and saying
// nothing is the honest output.
func crossings(engines []string, levels []int64, at map[string]map[int64]*float64) []string {
	var out []string
	for i := 0; i+1 < len(levels); i++ {
		for a := 0; a < len(engines); a++ {
			for b := a + 1; b < len(engines); b++ {
				before, ok := lead(at[engines[a]][levels[i]], at[engines[b]][levels[i]])
				after, ok2 := lead(at[engines[a]][levels[i+1]], at[engines[b]][levels[i+1]])
				if !ok || !ok2 || before == after {
					continue
				}
				winner, loser := engines[a], engines[b]
				if !after {
					winner, loser = loser, winner
				}
				out = append(out, fmt.Sprintf("cruzamento entre %d e %d leilões: %s passa %s",
					levels[i], levels[i+1], engineName[winner], engineName[loser]))
			}
		}
	}
	return out
}

// lead answers "is the first ahead of the second", and refuses to answer when
// either number does not exist or the two are equal — a cell that did not
// converge takes part in no crossing, because there is nothing to compare.
func lead(a, b *float64) (bool, bool) {
	if a == nil || b == nil || *a == *b {
		return false, false
	}
	return *a > *b, true
}

// Wide enough for "Single-writer" and for "1000 leilões", counted in runes:
// every name in this table has an accent in it, and %-13s pads by bytes.
const (
	labelWidth  = 15
	columnWidth = 14
)

func padLeft(s string, w int) string {
	if n := w - utf8.RuneCountInString(s); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}

func padRight(s string, w int) string {
	if n := w - utf8.RuneCountInString(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
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
