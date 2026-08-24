package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// No testdata/ with 37 directories enters the repository: a helper builds the
// matrix in memory and writes the three JSON files per cell into t.TempDir(),
// and it is that helper the cases mutate. A fixture of 111 files on disk would
// be a fixture nobody could review, and mutating one of them by hand is exactly
// the kind of edit that goes in wrong and passes.
const testCommit = "0123456789abcdef0123456789abcdef01234567"

type synth struct {
	t     *testing.T
	cells map[string]*synthCell
}

type synthCell struct {
	env     map[string]any
	client  map[string]any
	checker map[string]any
	absent  bool
}

func newSynth(t *testing.T) *synth {
	t.Helper()
	s := &synth{t: t, cells: map[string]*synthCell{}}
	for _, p := range plan() {
		s.cells[p.Name] = &synthCell{
			env:     baseEnv(p),
			client:  baseClient(p),
			checker: baseChecker(),
		}
	}
	return s
}

func baseEnv(p planned) map[string]any {
	return map[string]any{
		"run": "mtest/" + p.Name,
		"git": map[string]any{"commit": testCommit, "dirty": false},
		"cell": map[string]any{
			"strategy": p.Strategy, "auctions": p.Auctions,
			"policy": p.Policy, "scenario": p.Scenario, "poolSize": 25,
		},
		"host":      map[string]any{"kernel": "Linux test", "cpus": 16, "memoryBytes": 0},
		"limits":    map[string]any{"auctiond": map[string]any{"cpus": 4, "memoryBytes": 0}},
		"generator": map[string]any{"cpuPctPeak": 12.5, "saturated": false},
	}
}

func baseClient(p planned) map[string]any {
	// The control has to repeat cell 01's number, or every case below would
	// start out already refused by R9.
	accepted := int64(100 + p.Order)
	if p.Order == 37 {
		accepted = 101
	}
	return map[string]any{
		"run": "mtest/" + p.Name, "strategy": p.Strategy, "auctions": p.Auctions,
		"policy": p.Policy, "scenario": p.Scenario,
		"accepted": accepted, "conflict": accepted * 2, "exhausted": int64(10),
		"attempts": accepted * 3, "maxSeqSeen": accepted,
		"durationMs":       120000.0,
		"confirmLatencyMs": map[string]any{"avg": 100.0, "p95": 233.9, "max": 300.0},
	}
}

func baseChecker() map[string]any {
	return map[string]any{"findings": []any{}, "warnings": 0, "failures": 0, "exit": 0}
}

func (s *synth) cell(name string) *synthCell {
	s.t.Helper()
	c, ok := s.cells[name]
	if !ok {
		s.t.Fatalf("célula inexistente no plano: %s", name)
	}
	return c
}

// at finds the one cell whose name starts with the two-digit order, so the
// cases can say "19" instead of repeating a forty-character directory name.
func (s *synth) at(prefix string) *synthCell {
	s.t.Helper()
	for name := range s.cells {
		if strings.HasPrefix(name, prefix+"-") {
			return s.cells[name]
		}
	}
	s.t.Fatalf("nenhuma célula começando em %s", prefix)
	return nil
}

func (s *synth) write() string {
	s.t.Helper()
	dir := s.t.TempDir()
	for name, c := range s.cells {
		if c.absent {
			continue
		}
		cellDir := filepath.Join(dir, name)
		if err := os.MkdirAll(cellDir, 0o755); err != nil {
			s.t.Fatal(err)
		}
		for file, content := range map[string]map[string]any{
			"env.json": c.env, "client.json": c.client, "checker.json": c.checker,
		} {
			if content == nil {
				continue
			}
			data, err := json.MarshalIndent(content, "", "  ")
			if err != nil {
				s.t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cellDir, file), data, 0o644); err != nil {
				s.t.Fatal(err)
			}
		}
	}
	return dir
}

func (s *synth) run() (int, string, string) {
	s.t.Helper()
	dir := s.write()
	var out bytes.Buffer
	code := execute(dir, "", &out)
	return code, out.String(), dir
}

func published(t *testing.T, dir string) matrix {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "matrix.json"))
	if err != nil {
		t.Fatalf("matrix.json não foi publicado: %v", err)
	}
	var m matrix
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("a saída não menciona %q:\n%s", want, out)
		}
	}
}

// ------------------------------------------------------------------ casos ----

func TestCoherentMatrix(t *testing.T) {
	code, out, dir := newSynth(t).run()
	if code != exitOK {
		t.Fatalf("exit = %d, queria 0\n%s", code, out)
	}
	m := published(t, dir)
	if !m.Publishable {
		t.Error("publishable = false numa matriz coerente")
	}
	if len(m.Cells) != 36 {
		t.Errorf("linhas = %d, queria 36", len(m.Cells))
	}
	if m.Control == nil || m.Control.Verdict != "OK" {
		t.Errorf("controle = %+v, queria veredito OK", m.Control)
	}
	if m.PoolSize != 25 || m.Git.Commit != testCommit || m.Git.Dirty {
		t.Errorf("carimbo = pool %d commit %s dirty %v", m.PoolSize, m.Git.Commit, m.Git.Dirty)
	}
	// The order of the plan, kept: contention outermost, strategy innermost.
	for i, c := range m.Cells {
		if c.Order != i+1 {
			t.Fatalf("linha %d traz order=%d: a ordem do plano se perdeu", i, c.Order)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "matrix.md")); err != nil {
		t.Errorf("matrix.md não foi publicado: %v", err)
	}
	// Pasteable into benchmark.md means the header has to be that table's.
	md, err := os.ReadFile(filepath.Join(dir, "matrix.md"))
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(md), "| Estratégia | Leilões | Retry | VUs no pico | Aceitos/s |",
		"| Otimista |", "| Pessimista |", "| Single-writer |", "## Controle")
	// Header, separator, and 36 rows of data.
	if n := strings.Count(string(md), "\n| "); n != 38 {
		t.Errorf("a tabela tem %d linhas, queria 36 mais cabeçalho e separador", n)
	}
}

func TestMissingCell(t *testing.T) {
	s := newSynth(t)
	s.at("19").absent = true
	code, out, _ := s.run()
	if code != exitUnpublishable {
		t.Fatalf("exit = %d, queria 2\n%s", code, out)
	}
	mustContain(t, out, "R1", "19-", "célula faltando")
}

func TestIntruderDirectory(t *testing.T) {
	s := newSynth(t)
	dir := s.write()
	if err := os.MkdirAll(filepath.Join(dir, "38-shard-a1-ramp-immediate"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := execute(dir, "", &out); code != exitUnpublishable {
		t.Fatalf("exit = %d, queria 2\n%s", code, out.String())
	}
	mustContain(t, out.String(), "R1", "38-shard-a1-ramp-immediate", "fora do plano")
}

func TestCheckerUnverifiable(t *testing.T) {
	s := newSynth(t)
	s.at("08").checker["exit"] = 2
	code, out, _ := s.run()
	if code != exitUnpublishable {
		t.Fatalf("exit = %d, queria 2\n%s", code, out)
	}
	mustContain(t, out, "R2", "08-", "não verificada")
}

// The distinction cmd/checker has kept since etapa 1 has to survive the new
// layer: a violated invariant is a result about the engine, and it leaves as 1.
func TestCheckerFailuresExitOne(t *testing.T) {
	s := newSynth(t)
	s.at("14").checker["exit"] = 1
	s.at("14").checker["failures"] = 1
	code, out, dir := s.run()
	if code != exitViolated {
		t.Fatalf("exit = %d, queria 1\n%s", code, out)
	}
	m := published(t, dir)
	if m.Publishable {
		t.Error("publishable = true com invariante violado")
	}
	mustContain(t, string(mustRead(t, filepath.Join(dir, "matrix.md"))), "FALHA (1)")
}

func TestChaosBlockPresent(t *testing.T) {
	s := newSynth(t)
	s.at("21").checker["chaos"] = map[string]any{"scenario": "redis-pause", "landed": true}
	code, out, _ := s.run()
	if code != exitUnpublishable {
		t.Fatalf("exit = %d, queria 2\n%s", code, out)
	}
	mustContain(t, out, "R3", "21-", "mediu outro sistema")
}

func TestDivergentCommit(t *testing.T) {
	s := newSynth(t)
	s.at("30").env["git"] = map[string]any{"commit": "ffffffffffffffffffffffffffffffffffffffff", "dirty": false}
	code, out, _ := s.run()
	if code != exitUnpublishable {
		t.Fatalf("exit = %d, queria 2\n%s", code, out)
	}
	mustContain(t, out, "R4", "30-", "ffffffffffff")
}

func TestDirtyTree(t *testing.T) {
	s := newSynth(t)
	s.at("02").env["git"] = map[string]any{"commit": testCommit, "dirty": true}
	code, out, _ := s.run()
	if code != exitUnpublishable {
		t.Fatalf("exit = %d, queria 2\n%s", code, out)
	}
	mustContain(t, out, "R4", "02-", "árvore suja")
}

// The extreme case of decisão 107: a perfectly green cell that ran against
// another engine. Nothing below this layer can see it.
func TestStrategySwapped(t *testing.T) {
	s := newSynth(t)
	c := s.at("04") // 04-optimistic-a1-ramp-jitter
	c.env["cell"].(map[string]any)["strategy"] = "pessimistic"
	code, out, _ := s.run()
	if code != exitUnpublishable {
		t.Fatalf("exit = %d, queria 2\n%s", code, out)
	}
	mustContain(t, out, "R5", "04-", "strategy=pessimistic")
}

func TestPoolDivergent(t *testing.T) {
	s := newSynth(t)
	s.at("25").env["cell"].(map[string]any)["poolSize"] = 50
	code, out, _ := s.run()
	if code != exitUnpublishable {
		t.Fatalf("exit = %d, queria 2\n%s", code, out)
	}
	mustContain(t, out, "R6", "25-", "poolSize 50")
}

func TestDurationMissingOrZero(t *testing.T) {
	for name, mutate := range map[string]func(*synthCell){
		"ausente": func(c *synthCell) { delete(c.client, "durationMs") },
		"zero":    func(c *synthCell) { c.client["durationMs"] = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			s := newSynth(t)
			mutate(s.at("11"))
			code, out, _ := s.run()
			if code != exitUnpublishable {
				t.Fatalf("exit = %d, queria 2\n%s", code, out)
			}
			mustContain(t, out, "R7", "11-")
		})
	}
}

// The refusal only this layer can make: the checker.json is entirely green,
// because zero accepts violates no invariant. The row would still be all zeros
// and a division by zero.
func TestAcceptedZero(t *testing.T) {
	s := newSynth(t)
	s.at("12").client["accepted"] = 0
	code, out, _ := s.run()
	if code != exitUnpublishable {
		t.Fatalf("exit = %d, queria 2\n%s", code, out)
	}
	mustContain(t, out, "R8", "12-", "accepted=0")
}

func TestControlWithinBand(t *testing.T) {
	s := newSynth(t)
	s.at("37").client["accepted"] = int64(113) // 11.9% acima de 101
	code, out, dir := s.run()
	if code != exitOK {
		t.Fatalf("exit = %d, queria 0\n%s", code, out)
	}
	m := published(t, dir)
	if m.Control == nil || m.Control.Verdict != "AVISO" {
		t.Fatalf("controle = %+v, queria AVISO", m.Control)
	}
	if len(m.Warnings) != 1 || !strings.Contains(m.Warnings[0], "controle divergente") {
		t.Errorf("avisos da matriz = %v", m.Warnings)
	}
	mustContain(t, out, "## Controle", "AVISO")
}

func TestControlOutOfBand(t *testing.T) {
	s := newSynth(t)
	s.at("37").client["accepted"] = int64(132) // 30.7% acima de 101
	code, out, _ := s.run()
	if code != exitUnpublishable {
		t.Fatalf("exit = %d, queria 2\n%s", code, out)
	}
	mustContain(t, out, "R9", "ordem de execução")
}

func TestExhaustionWarning(t *testing.T) {
	s := newSynth(t)
	c := s.at("07")
	c.client["exhausted"] = int64(36) // 36/(107+36) = 25.2%
	code, out, dir := s.run()
	if code != exitOK {
		t.Fatalf("exit = %d, queria 0\n%s", code, out)
	}
	m := published(t, dir)
	if got := m.Cells[6].Warnings; len(got) != 1 || !strings.Contains(got[0], "exaustão") {
		t.Fatalf("avisos da linha 07 = %v", got)
	}
	for i, cell := range m.Cells {
		if i != 6 && len(cell.Warnings) != 0 {
			t.Errorf("o aviso vazou para a linha %d: %v", cell.Order, cell.Warnings)
		}
	}
	mustContain(t, out, "célula 07 · exaustão acima de 20%")
}

func TestGeneratorSaturatedWarning(t *testing.T) {
	s := newSynth(t)
	s.at("33").env["generator"] = map[string]any{"cpuPctPeak": 96.0, "saturated": true}
	code, out, dir := s.run()
	if code != exitOK {
		t.Fatalf("exit = %d, queria 0\n%s", code, out)
	}
	m := published(t, dir)
	if got := m.Cells[32].Warnings; len(got) != 1 || got[0] != "gerador saturado" {
		t.Fatalf("avisos da linha 33 = %v", got)
	}
	mustContain(t, out, "célula 33 · gerador saturado")
}

// A matrix with three problems reporting one per run costs three runs to find
// the third, and each run that needs cells re-measured costs hours.
func TestThreeProblemsAtOnce(t *testing.T) {
	s := newSynth(t)
	s.at("05").env["git"] = map[string]any{"commit": testCommit, "dirty": true}
	s.at("19").absent = true
	s.at("22").env["cell"].(map[string]any)["poolSize"] = 50
	code, out, _ := s.run()
	if code != exitUnpublishable {
		t.Fatalf("exit = %d, queria 2\n%s", code, out)
	}
	mustContain(t, out, "R4", "05-", "R1", "19-", "R6", "22-")
	if n := strings.Count(out, "\n  R"); n != 3 {
		t.Errorf("relatou %d recusas, queria as três:\n%s", n, out)
	}
}

// bin/matrix is a pure function of the 37 directories: no database, no network,
// no writing inside a cell.
func TestIdempotent(t *testing.T) {
	s := newSynth(t)
	dir := s.write()
	var first, second bytes.Buffer
	if code := execute(dir, "", &first); code != exitOK {
		t.Fatalf("primeira execução saiu %d\n%s", code, first.String())
	}
	a := mustRead(t, filepath.Join(dir, "matrix.json"))
	if code := execute(dir, "", &second); code != exitOK {
		t.Fatalf("segunda execução saiu %d\n%s", code, second.String())
	}
	b := mustRead(t, filepath.Join(dir, "matrix.json"))

	stamp := regexp.MustCompile(`"generatedAt": "[^"]*"`)
	if x, y := stamp.ReplaceAllString(string(a), ""), stamp.ReplaceAllString(string(b), ""); x != y {
		t.Error("matrix.json mudou entre duas execuções sobre o mesmo diretório")
	}
	if first.String() != second.String() {
		t.Error("a tabela do stdout mudou entre duas execuções")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
