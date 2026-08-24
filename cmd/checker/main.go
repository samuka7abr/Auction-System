// Command checker proves, after one benchmark cell, the invariants that have to
// hold whichever strategy produced it.
//
// It never asks Prometheus (decisão 4): comparing bids against
// bid_outcomes_total{outcome="accepted"} would be auctiond checking itself, and
// a handler that lies makes the metric lie with it. The only independent source
// is the client, and client.json is where this starts.
//
// No query is scoped by run. Decisão 13 resets the database before every cell,
// so the tables hold exactly one cell when this runs — which means no query
// needs a filter, and a forgotten filter cannot hide a violation.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Exit codes. Both 1 and 2 stop the matrix of etapa 5, and they are distinct
// because only one of them is a result: a violated invariant says something
// about the engine, a database that would not answer says nothing at all.
const (
	exitOK           = 0
	exitViolated     = 1
	exitUnverifiable = 2
)

type verdict string

const (
	verdictOK   verdict = "OK"
	verdictWarn verdict = "WARN"
	verdictFail verdict = "FAIL"
)

// finding is one line of the report.
type finding struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Verdict verdict `json:"verdict"`
	Detail  string  `json:"detail,omitempty"`
}

type report struct {
	Run string `json:"run"`
	// Chaos is absent in every cell of the matrix and present in the five of
	// spec 02. It is carried into the JSON so that a row nobody may compare
	// against a healthy one says so in its own artefact (decisão 92).
	Chaos    *chaosReport `json:"chaos,omitempty"`
	Findings []finding    `json:"findings"`
	Warnings int          `json:"warnings"`
	Failures int          `json:"failures"`
	Exit     int          `json:"exit"`
}

func main() {
	var (
		run     string
		results string
		asJSON  bool
	)
	flag.StringVar(&run, "run", "", "id of the cell to verify")
	flag.StringVar(&results, "results", "bench/results", "directory holding the cells' artefacts")
	flag.BoolVar(&asJSON, "json", false, "also write the report as JSON next to the text one")
	flag.Parse()

	os.Exit(execute(run, results, asJSON, os.Stdout))
}

func execute(run, results string, asJSON bool, out io.Writer) int {
	if run == "" {
		return unverifiable(out, fmt.Errorf("-run is required"))
	}
	dir := filepath.Join(results, run)

	// The two client-side files are read before Postgres is dialled: a missing
	// client.json is exit 2 either way, and finding that out without waiting on
	// a connection keeps the message about the real cause.
	client, err := readClient(dir)
	if err != nil {
		return unverifiable(out, err)
	}
	env, err := readEnv(dir)
	if err != nil {
		return unverifiable(out, err)
	}
	// Absent is the normal cell and the only artefact of this harness whose
	// absence means something rather than nothing.
	chaos, err := readChaos(dir)
	if err != nil {
		return unverifiable(out, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return unverifiable(out, fmt.Errorf("open database: %w", err))
	}
	defer pool.Close()

	sqlFindings, totals, err := checkSQL(ctx, pool)
	if err != nil {
		return unverifiable(out, err)
	}
	// I7 is proved in SQL with I1..I4, but the public report stays numerically
	// ordered: the two client-aware checks sit before it.
	findings := make([]finding, 0, len(sqlFindings)+2)
	for _, f := range sqlFindings {
		if f.ID == "I7" {
			findings = append(findings, checkDurability(totals, client, env, chaos), checkCellValidity(client, env, chaos))
		}
		findings = append(findings, f)
	}

	rep := summarize(run, findings)
	rep.Chaos = chaos
	render(out, rep)
	if asJSON {
		if err := writeJSON(filepath.Join(dir, "checker.json"), rep); err != nil {
			fmt.Fprintln(out, "checker: could not write the JSON report:", err)
		}
	}
	return rep.Exit
}

// summarize turns the findings into an exit code.
//
// A FAIL outranks a query that could not run: a violation actually found is the
// strongest statement available, and it is a result about the engine. Exit 2 is
// reserved for a run that produced no verdict at all.
func summarize(run string, findings []finding) report {
	rep := report{Run: run, Findings: findings}
	for _, f := range findings {
		switch f.Verdict {
		case verdictWarn:
			rep.Warnings++
		case verdictFail:
			rep.Failures++
		}
	}
	if rep.Failures > 0 {
		rep.Exit = exitViolated
	}
	return rep
}

const (
	idWidth      = 4
	nameWidth    = 32
	verdictWidth = 6
)

func render(out io.Writer, rep report) {
	// Above the verdicts, never below: a reader who stops at the first line has
	// to know that a process was killed inside this cell.
	if rep.Chaos != nil {
		fmt.Fprintln(out, rep.Chaos.line())
	}
	for _, f := range rep.Findings {
		line := pad(f.ID, idWidth) + pad(f.Name, nameWidth) + pad(string(f.Verdict), verdictWidth) + f.Detail
		fmt.Fprintln(out, strings.TrimRight(line, " "))
	}
	switch {
	case rep.Failures > 0:
		fmt.Fprintf(out, "resultado: FALHA — %s\n", plural(rep.Failures, "invariante violado", "invariantes violados"))
	case rep.Warnings > 0:
		fmt.Fprintf(out, "resultado: OK com %s\n", plural(rep.Warnings, "aviso", "avisos"))
	default:
		fmt.Fprintln(out, "resultado: OK")
	}
}

func unverifiable(out io.Writer, err error) int {
	fmt.Fprintf(out, "resultado: NÃO VERIFICADO — %v\n", err)
	return exitUnverifiable
}

func writeJSON(path string, rep report) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// pad counts runes, not bytes: the report is in Portuguese and %-32s would
// misalign every accented label.
func pad(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s + " "
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
