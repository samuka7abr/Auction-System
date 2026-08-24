// Command matrix turns the 37 directories one run of bench/run-matrix.sh leaves
// behind into a single artefact etapa 6 can read — or into a named refusal.
//
// It reads three files per cell, env.json, client.json and checker.json, and
// nothing else. Never summary.json, whose shape is internal to k6 and moves
// between versions in silence; never Prometheus, because asking the server for
// the number the server is being judged on is auctiond checking itself; never
// the database, which by now holds only the last cell (decisões 4 and 96).
//
// Half of what is here exists to reprove the matrix. Five of the nine refusals
// are statements cmd/checker is structurally unable to make: it verifies one
// cell at a time, against a database that was just reset, and it cannot know
// which directory the cell landed in, which commit produced the neighbouring
// cell, or what the other 36 did. A perfectly green cell whose env.json says
// pessimistic and whose directory says optimistic is a plausible, false row,
// and this is the only layer that can see it (decisão 107).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Exit codes, and the distinction is the checker's, kept alive across a new
// layer: a violated invariant is a result about the engine, an incoherent
// matrix is the absence of a result. The layer that erased it would be the
// layer decisão 93 already stopped `make` from being.
const (
	exitOK            = 0
	exitViolated      = 1
	exitUnpublishable = 2
)

// The bands of decisão 95. The control does not separate drift from noise — it
// measures the two added together — so a band is the only honest way to read
// an instrument that measures two things at once. Above 25% the distance
// between two curves of the main graph could be entirely explained by the
// order the cells ran in.
const (
	controlWarn   = 0.10
	controlRefuse = 0.25
)

// The threshold benchmark.md sets for exhaustion: above it MAX_RETRIES is
// masking the effect. Raising MAX_RETRIES means running all 36 cells again, so
// this is a mark on the row and a decision for etapa 6 (decisão 106).
const exhaustionWarn = 0.20

func main() {
	var dir, out, kind string
	flag.StringVar(&dir, "dir", "", "directory holding one matrix's cells")
	flag.StringVar(&out, "out", "", "where matrix.json and matrix.md go (default: -dir)")
	flag.StringVar(&kind, "plan", planFull, "which plan the directory was measured against: full or slice")
	flag.Parse()

	os.Exit(execute(dir, out, kind, os.Stdout))
}

func execute(dir, out, kind string, w io.Writer) int {
	if dir == "" {
		fmt.Fprintln(w, "matriz NÃO PUBLICÁVEL:")
		fmt.Fprintln(w, "  -dir é obrigatório")
		return exitUnpublishable
	}
	if kind != planFull && kind != planSlice {
		fmt.Fprintln(w, "matriz NÃO PUBLICÁVEL:")
		fmt.Fprintf(w, "  -plan é %s ou %s, não %q\n", planFull, planSlice, kind)
		return exitUnpublishable
	}
	if out == "" {
		out = dir
	}

	m, refusals := read(dir, kind)

	// All of them, never only the first: a matrix with three problems reporting
	// one per run costs three runs to find the third, and each run that needs
	// cells re-measured costs hours (decisão 107).
	if len(refusals) > 0 {
		fmt.Fprintln(w, "matriz NÃO PUBLICÁVEL:")
		for _, r := range refusals {
			fmt.Fprintln(w, "  "+r)
		}
		if m.Failures > 0 {
			fmt.Fprintf(w, "e %s com invariante violado\n", plural(m.Failures, "célula", "células"))
			return exitViolated
		}
		return exitUnpublishable
	}

	if err := publish(m, out); err != nil {
		fmt.Fprintln(w, "matriz NÃO PUBLICÁVEL:")
		fmt.Fprintln(w, "  "+err.Error())
		return exitUnpublishable
	}
	fmt.Fprint(w, markdown(m))

	// Same rule the checker has kept since etapa 1: a violation actually found
	// outranks everything, because it is the strongest statement available and
	// it is a result about the engine (decisão 107).
	if m.Failures > 0 {
		return exitViolated
	}
	return exitOK
}

// ------------------------------------------------------------------ plan ----

// planned is one row of the plan, and the aggregator has to know the plan for
// the same reason the loop does: a matrix of 35 cells with a gap draws no
// curve, and a 38th directory is a cell of unknown origin.
type planned struct {
	Order    int
	Name     string
	Strategy string
	Auctions int64
	Scenario string
	Policy   string
}

// The two plans a directory can have been measured against. The nine refusals
// run against whichever one is named, because R1 counts the cells OF A PLAN: a
// slice judged as a full matrix is 27 refusals for cells nobody ran.
const (
	planFull  = "full"
	planSlice = "slice"
)

// The order of decisão 94: contention outermost, strategy innermost. Cell 37 is
// cell 01 again, and it is the control.
//
// The slice is the main graph and nothing else — throughput by contention, one
// line per strategy — which is a function of two variables and therefore of
// nine cells, at one scenario and one policy. It keeps the numbers those cells
// have in the full plan, so a cell measured in a slice and the same cell of a
// future full matrix are comparable without renaming anything (spec 02).
func plan(kind string) []planned {
	var p []planned
	order := 1
	for _, auctions := range []int64{1, 10, 1000} {
		for _, scenario := range []string{"ramp", "last_second_spike"} {
			for _, policy := range []string{"immediate", "jitter"} {
				for _, strategy := range []string{"optimistic", "pessimistic", "shard"} {
					// The order is the cell's number in the plan of 37, counted
					// before the slice drops anything: the slice selects cells,
					// it does not renumber them.
					n := order
					order++
					if kind == planSlice && (scenario != "ramp" || policy != "immediate") {
						continue
					}
					p = append(p, planned{
						Order:    n,
						Name:     fmt.Sprintf("%02d-%s-a%d-%s-%s", n, strategy, auctions, scenario, policy),
						Strategy: strategy, Auctions: auctions, Scenario: scenario, Policy: policy,
					})
				}
			}
		}
	}
	return append(p, planned{
		Order: 37, Name: "37-control-optimistic-a1-ramp-immediate",
		Strategy: "optimistic", Auctions: 1, Scenario: "ramp", Policy: "immediate",
	})
}

// peakVUs derives from the scenario, and an unknown scenario is a refusal
// rather than a zero: a row whose peak load is unknown is not a row.
func peakVUs(scenario string) (int, bool) {
	switch scenario {
	case "ramp":
		return 500, true
	case "last_second_spike":
		return 1000, true
	}
	return 0, false
}

// -------------------------------------------------------------- artefacts ----

// envReport is the environment half. host and limits travel as raw bytes: they
// are identical across the 37 by construction, R4 is what guarantees it, and
// modelling them here would be a second schema to keep in step with env.sh for
// no gain.
type envReport struct {
	Run       string           `json:"run"`
	Git       *gitReport       `json:"git"`
	Cell      *cellReport      `json:"cell"`
	Host      json.RawMessage  `json:"host"`
	Limits    json.RawMessage  `json:"limits"`
	Generator *generatorReport `json:"generator"`
}

type gitReport struct {
	Commit string `json:"commit"`
	Dirty  *bool  `json:"dirty"`
}

type cellReport struct {
	Strategy string `json:"strategy"`
	Auctions *int64 `json:"auctions"`
	Policy   string `json:"policy"`
	Scenario string `json:"scenario"`
	PoolSize *int64 `json:"poolSize"`
	// Absent means a cell measured before etapa 5 gave the harness a budget,
	// and a cell nobody interrupted — the two are the same statement here.
	Interrupted *bool `json:"interrupted"`
}

// interrupted is the one question the checker cannot answer about a cell: it
// verifies correctness, and a cell watched for less time is not an incorrect
// cell. It is also not a failure — it is a cell that did not converge, and it
// goes into the table by name instead of by rate (spec 02).
func interrupted(c *loaded) bool {
	return c != nil && c.env.Cell != nil && c.env.Cell.Interrupted != nil && *c.env.Cell.Interrupted
}

type generatorReport struct {
	Saturated *bool `json:"saturated"`
}

// clientReport inherits the whole contract of cmd/checker/client.go, pointers
// included: absent cannot be read as zero. A client.json k6 could not finish
// building has to stop the matrix here, and not three columns later as a green
// rate computed over nothing.
type clientReport struct {
	Accepted         *int64       `json:"accepted"`
	Conflict         *int64       `json:"conflict"`
	Exhausted        *int64       `json:"exhausted"`
	Attempts         *int64       `json:"attempts"`
	DurationMs       *float64     `json:"durationMs"`
	ConfirmLatencyMs *trendReport `json:"confirmLatencyMs"`
}

type trendReport struct {
	P95 *float64 `json:"p95"`
}

type checkerReport struct {
	// Present means the cell measured another system, and its numbers may not
	// share a table with a healthy cell's (decisão 92).
	Chaos    json.RawMessage `json:"chaos"`
	Warnings *int            `json:"warnings"`
	Failures *int            `json:"failures"`
	Exit     *int            `json:"exit"`
}

func readJSON(path string, into any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

// ------------------------------------------------------------- as recusas ----

type reader struct {
	dir      string
	refusals []string
}

func (r *reader) refuse(id, name, format string, args ...any) {
	where := ""
	if name != "" {
		where = " " + name + ":"
	}
	r.refusals = append(r.refusals, fmt.Sprintf("%s ·%s %s", id, where, fmt.Sprintf(format, args...)))
}

func read(dir, kind string) (matrix, []string) {
	r := &reader{dir: dir}
	p := plan(kind)
	m := matrix{Matrix: filepath.Base(dir)}

	present := r.checkDirectories(p)

	cells := make(map[string]*loaded, len(present))
	for _, want := range p {
		if !present[want.Name] {
			continue
		}
		if c := r.load(want); c != nil {
			cells[want.Name] = c
		}
	}

	r.checkCoherence(p, cells)
	m.Warnings = []string{}
	m.Cells = r.rows(p, cells, &m)
	r.control(p, cells, &m)

	if len(r.refusals) == 0 {
		r.stamp(p, cells, &m)
	}
	// Sorted last, and only here: rows and control can still refuse. Two runs
	// over the same directory have to report the same thing in the same order,
	// which is the whole of the idempotence this binary promises.
	sort.Strings(r.refusals)
	m.Publishable = len(r.refusals) == 0 && m.Failures == 0
	return m, r.refusals
}

// checkDirectories is R1, in both directions: a cell of the plan with no
// directory, and a directory the plan never asked for.
func (r *reader) checkDirectories(p []planned) map[string]bool {
	wanted := make(map[string]bool, len(p))
	for _, want := range p {
		wanted[want.Name] = true
	}

	entries, err := os.ReadDir(r.dir)
	if err != nil {
		r.refuse("R1", "", "não consegui ler %s: %v", r.dir, err)
		return nil
	}

	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !wanted[e.Name()] {
			r.refuse("R1", e.Name(), "diretório fora do plano: célula de origem desconhecida")
			continue
		}
		present[e.Name()] = true
	}
	for _, want := range p {
		if !present[want.Name] {
			r.refuse("R1", want.Name, "célula faltando: uma matriz com lacuna não desenha curva")
		}
	}
	return present
}

// loaded is one cell's three artefacts, already decoded.
type loaded struct {
	env     envReport
	client  clientReport
	checker checkerReport
}

func (r *reader) load(want planned) *loaded {
	dir := filepath.Join(r.dir, want.Name)
	var c loaded
	ok := true
	for file, into := range map[string]any{
		"env.json":     &c.env,
		"client.json":  &c.client,
		"checker.json": &c.checker,
	} {
		if err := readJSON(filepath.Join(dir, file), into); err != nil {
			r.refuse("R2", want.Name, "%v", err)
			ok = false
		}
	}
	if !ok {
		return nil
	}
	return &c
}

// checkCoherence is everything that only makes sense read across cells: R2, R3,
// R4, R5, R6, R7 and R8.
func (r *reader) checkCoherence(p []planned, cells map[string]*loaded) {
	var commit string
	var pool *int64
	var commitFrom, poolFrom string

	for _, want := range p {
		c, ok := cells[want.Name]
		if !ok {
			continue
		}
		name := want.Name

		// R2 — a cell that was not verified is not a result (decisão 93). exit 1
		// is a result, and it leaves through Failures below.
		switch {
		case c.checker.Exit == nil:
			r.refuse("R2", name, "checker.json sem exit")
		case *c.checker.Exit == exitUnpublishable:
			r.refuse("R2", name, "checker.json com exit 2: célula não verificada")
		case *c.checker.Exit != exitOK && *c.checker.Exit != exitViolated:
			r.refuse("R2", name, "checker.json com exit %d: código desconhecido", *c.checker.Exit)
		}
		if c.checker.Failures == nil || c.checker.Warnings == nil {
			r.refuse("R2", name, "checker.json sem failures ou warnings")
		}

		// R3 — a chaos cell measured another system.
		if len(c.checker.Chaos) > 0 && string(c.checker.Chaos) != "null" {
			r.refuse("R3", name, "bloco chaos presente: esta célula mediu outro sistema")
		}

		// R4 — two cells built from different commits are not one matrix, and a
		// number measured from a dirty tree is not reproducible.
		switch {
		case c.env.Git == nil || c.env.Git.Commit == "" || c.env.Git.Dirty == nil:
			r.refuse("R4", name, "env.json sem git.commit ou git.dirty")
		default:
			if *c.env.Git.Dirty {
				r.refuse("R4", name, "medida de árvore suja (dirty: true)")
			}
			if commit == "" {
				commit, commitFrom = c.env.Git.Commit, name
			} else if c.env.Git.Commit != commit {
				r.refuse("R4", name, "commit %s, e %s foi medida em %s",
					short(c.env.Git.Commit), commitFrom, short(commit))
			}
		}

		// R5 — the extreme case: a perfectly green cell that ran against another
		// engine, or a directory somebody reused. The row it would produce is
		// plausible and false, and nothing below this layer can see it.
		if c.env.Cell == nil {
			r.refuse("R5", name, "env.json sem o bloco cell")
		} else {
			cell := c.env.Cell
			if cell.Strategy != want.Strategy {
				r.refuse("R5", name, "env.json diz strategy=%s e o diretório diz %s", cell.Strategy, want.Strategy)
			}
			if cell.Scenario != want.Scenario {
				r.refuse("R5", name, "env.json diz scenario=%s e o diretório diz %s", cell.Scenario, want.Scenario)
			}
			if cell.Policy != want.Policy {
				r.refuse("R5", name, "env.json diz policy=%s e o diretório diz %s", cell.Policy, want.Policy)
			}
			if cell.Auctions == nil {
				r.refuse("R5", name, "env.json sem cell.auctions")
			} else if *cell.Auctions != want.Auctions {
				r.refuse("R5", name, "env.json diz auctions=%d e o diretório diz %d", *cell.Auctions, want.Auctions)
			}

			// R6 — the pool is the sweep of spec 02, and here it is constant. The
			// likeliest way the two get mixed is a variable left in a shell, and
			// env.json is the only place that would record it (decisão 105).
			if cell.PoolSize == nil {
				r.refuse("R6", name, "env.json sem cell.poolSize")
			} else if pool == nil {
				pool, poolFrom = cell.PoolSize, name
			} else if *cell.PoolSize != *pool {
				r.refuse("R6", name, "poolSize %d, e %s mediu com %d", *cell.PoolSize, poolFrom, *pool)
			}
		}

		if c.env.Generator == nil || c.env.Generator.Saturated == nil {
			r.refuse("R2", name, "env.json sem generator.saturated")
		}

		// R7 — every per-second rate of the row would come out zero or infinite.
		if c.client.DurationMs == nil {
			r.refuse("R7", name, "client.json sem durationMs: toda taxa por segundo sairia zero")
		} else if *c.client.DurationMs <= 0 {
			r.refuse("R7", name, "durationMs=%g: toda taxa por segundo sairia infinita", *c.client.DurationMs)
		}

		for label, v := range map[string]*int64{
			"accepted": c.client.Accepted, "conflict": c.client.Conflict,
			"exhausted": c.client.Exhausted, "attempts": c.client.Attempts,
		} {
			if v == nil {
				r.refuse("R7", name, "client.json sem %s: ausente não pode ser lido como zero", label)
			}
		}
		if c.client.ConfirmLatencyMs == nil || c.client.ConfirmLatencyMs.P95 == nil {
			r.refuse("R7", name, "client.json sem confirmLatencyMs.p95")
		}

		// R8 — the cheapest of the nine and the least likely to fire, because a
		// cell with zero accepts has no seq_seen sample and its client.json is
		// never written. It costs one comparison and it stops a division by zero
		// in the attempts-per-accept column.
		if c.client.Accepted != nil && *c.client.Accepted == 0 {
			r.refuse("R8", name, "accepted=0: toda taxa da linha sai zero e tentativas/aceito divide por zero")
		}
	}

}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func joinNonEmpty(parts []string, sep string) string {
	kept := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}
