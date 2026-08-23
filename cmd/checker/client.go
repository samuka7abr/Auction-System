package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// clientReport is the k6 half of the contract, and the reason it is a file of
// its own rather than the k6 summary: the shape of k6's `data` is internal to
// k6 and moves between versions. A checker that navigated it would break on an
// image upgrade, and break *quietly* if a field turned null instead of
// vanishing — the durability invariant would pass green against a zero.
//
// Every count is a pointer so that "absent" cannot be read as "zero". That is
// the whole point: a client.json the k6 could not build completely has to stop
// the cell here, with exit 2, instead of being verified against nothing.
type clientReport struct {
	Run      string `json:"run"`
	Strategy string `json:"strategy"`
	Auctions int64  `json:"auctions"`
	Policy   string `json:"policy"`
	Scenario string `json:"scenario"`

	Accepted   *int64 `json:"accepted"`
	Conflict   *int64 `json:"conflict"`
	Outbid     *int64 `json:"outbid"`
	Closed     *int64 `json:"closed"`
	Invalid    *int64 `json:"invalid"`
	Error      *int64 `json:"error"`
	Exhausted  *int64 `json:"exhausted"`
	Attempts   *int64 `json:"attempts"`
	MaxSeqSeen *int64 `json:"maxSeqSeen"`

	Replayed           *int64 `json:"replayed"`
	InFlight           *int64 `json:"inFlight"`
	DuplicatesSelected *int64 `json:"duplicatesSelected"`
	DuplicatesInjected *int64 `json:"duplicatesInjected"`
	TransportRetries   *int64 `json:"transportRetries"`
}

func (c clientReport) required() map[string]*int64 {
	return map[string]*int64{
		"accepted": c.Accepted, "conflict": c.Conflict, "outbid": c.Outbid,
		"closed": c.Closed, "invalid": c.Invalid, "error": c.Error,
		"exhausted": c.Exhausted, "attempts": c.Attempts, "maxSeqSeen": c.MaxSeqSeen,
		"replayed": c.Replayed, "inFlight": c.InFlight,
		"duplicatesSelected": c.DuplicatesSelected, "duplicatesInjected": c.DuplicatesInjected,
		"transportRetries": c.TransportRetries,
	}
}

// envReport is read for one number only: whether the generator was near its own
// limit while the cell ran. A saturated k6 measures k6.
type envReport struct {
	Generator *generatorReport `json:"generator"`
}

type generatorReport struct {
	CPUPctPeak *float64 `json:"cpuPctPeak"`
	Saturated  *bool    `json:"saturated"`
}

func readClient(dir string) (clientReport, error) {
	var c clientReport
	if err := readFile(filepath.Join(dir, "client.json"), &c); err != nil {
		return c, err
	}
	var missing []string
	for name, v := range c.required() {
		if v == nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return c, fmt.Errorf("client.json is missing %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func readEnv(dir string) (envReport, error) {
	var e envReport
	if err := readFile(filepath.Join(dir, "env.json"), &e); err != nil {
		return e, err
	}
	if e.Generator == nil || e.Generator.CPUPctPeak == nil || e.Generator.Saturated == nil {
		return e, fmt.Errorf("env.json is missing generator.cpuPctPeak or generator.saturated")
	}
	return e, nil
}

func readFile(path string, into any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

// checkDurability is I5. Idempotent transport recovery removes the etapa 1
// asymmetry: the durable history and the logical acceptances must now agree in
// both count and watermark (decisão 46).
//
// Under chaos the asymmetry comes back, and only in the safe direction: see
// aheadUnderChaos. The other direction — a confirmed bid the database does not
// have — is the failure this whole harness exists for, and it does not move.
func checkDurability(db cellTotals, c clientReport, chaos *chaosReport) finding {
	f := finding{ID: "I5", Name: "durabilidade db x cliente", Verdict: verdictOK}
	accepted, maxSeq := *c.Accepted, *c.MaxSeqSeen

	switch {
	case db.Bids < accepted:
		f.Verdict = verdictFail
		f.Detail = fmt.Sprintf("LANCE CONFIRMADO SUMIU: db=%d cliente=%d", db.Bids, accepted)
	case db.Bids > accepted:
		f.Verdict = aheadUnderChaos(chaos)
		f.Detail = fmt.Sprintf("BANCO À FRENTE: db=%d cliente=%d", db.Bids, accepted)
	default:
		f.Detail = fmt.Sprintf("db=%d cliente=%d", db.Bids, accepted)
	}

	// The watermark is global (emenda 28): the largest seq any VU saw confirmed,
	// in any auction. Together with I1's density it subsumes the per-auction
	// attribution — a durable 201 cannot vanish without either dropping the count
	// below the client's or opening a hole in some auction's sequence.
	//
	// It is read by direction for the same reason the counts are: a database
	// behind the client is a lost write whatever killed the process.
	switch {
	case db.MaxSeq < maxSeq:
		f.Verdict = worse(f.Verdict, verdictFail)
		f.Detail += fmt.Sprintf(" · watermark divergente: db=%d cliente=%d", db.MaxSeq, maxSeq)
	case db.MaxSeq > maxSeq:
		f.Verdict = worse(f.Verdict, aheadUnderChaos(chaos))
		f.Detail += fmt.Sprintf(" · watermark divergente: db=%d cliente=%d", db.MaxSeq, maxSeq)
	}
	return f
}

// aheadUnderChaos is half of decisão 85: the database holding more than the
// client counted is a 201 whose commit outran its own response, and killing the
// process that would have answered is exactly how that happens — the mark in
// Redis expires with no owner and the stored response is lost for good.
//
// Outside chaos there is no such excuse and it stays a failure: idempotent
// transport recovery is supposed to make every durable write observable.
func aheadUnderChaos(chaos *chaosReport) verdict {
	if chaos != nil {
		return verdictWarn
	}
	return verdictFail
}

// The share of requests that may fail for real before the cell stops being a
// measurement. Same number as the k6 threshold, and for the same reason: above
// it, what broke was the measurement and not the engine.
const maxErrorRate = 0.01

// checkCellValidity is I6: it does not judge the engine, it judges whether this
// cell is worth reading at all.
//
// It is the only check chaos turns around, and in both directions at once
// (decisão 85): the error rate stops failing the cell, because under an
// injected failure the error IS the injection and failing on it would be
// failing the experiment; and an injection that never landed starts failing it,
// because a chaos cell that broke nothing is the worst result available — it
// looks like proof and is not (decisões 59 and 84).
func checkCellValidity(c clientReport, e envReport, chaos *chaosReport) finding {
	f := finding{ID: "I6", Name: "célula válida", Verdict: verdictOK}
	var fails, warns []string

	attempts, invalid, errs, closed := *c.Attempts, *c.Invalid, *c.Error, *c.Closed
	replayed, inFlight := *c.Replayed, *c.InFlight
	selected, injected := *c.DuplicatesSelected, *c.DuplicatesInjected

	counters := []struct {
		name  string
		value int64
	}{
		{"accepted", *c.Accepted}, {"conflict", *c.Conflict}, {"outbid", *c.Outbid},
		{"closed", closed}, {"invalid", invalid}, {"error", errs},
		{"exhausted", *c.Exhausted}, {"attempts", attempts}, {"replayed", replayed},
		{"inFlight", inFlight}, {"duplicatesSelected", selected},
		{"duplicatesInjected", injected}, {"transportRetries", *c.TransportRetries},
	}
	for _, counter := range counters {
		if counter.value < 0 {
			fails = append(fails, fmt.Sprintf("%s=%d: contador negativo", counter.name, counter.value))
		}
	}

	if attempts == 0 {
		fails = append(fails, "nenhuma tentativa registrada")
	}
	if invalid > 0 {
		// 400 expected_version_required. Decisão 21 put this series here for
		// free: above zero it means the k6 sent a bid without expectedVersion.
		fails = append(fails, fmt.Sprintf("invalid=%d: k6 mal configurado", invalid))
	}
	if selected == 0 {
		fails = append(fails, "nenhuma duplicata selecionada")
	}
	if injected == 0 {
		fails = append(fails, "nenhuma duplicata injetada")
	}
	if replayed == 0 {
		fails = append(fails, "nenhum replay observado")
	}
	if injected > selected {
		fails = append(fails, fmt.Sprintf("duplicatas injetadas=%d acima das selecionadas=%d", injected, selected))
	}
	if inFlight == 0 {
		warns = append(warns, "in_flight=0: concorrente pode ter encontrado replay")
	}

	var rate float64
	if attempts > 0 {
		rate = float64(errs) / float64(attempts)
	}
	if rate >= maxErrorRate {
		if chaos == nil {
			fails = append(fails, fmt.Sprintf("erro=%s acima de %.0f%%: infra caindo", percent(rate), maxErrorRate*100))
		} else {
			warns = append(warns, fmt.Sprintf("erro=%s: é a injeção de %s", percent(rate), chaos.Scenario))
		}
	}
	if chaos != nil && !chaos.Landed {
		fails = append(fails, fmt.Sprintf("caos %s não aterrissou: a célula não prova nada", chaos.Scenario))
	}
	if *e.Generator.Saturated {
		// A warning and not a failure: discarding the cell is the reader's call.
		// But never in silence — above 90% of its own limit the number may be the
		// generator's rather than auctiond's.
		warns = append(warns, fmt.Sprintf("gerador=%.0f%% do limite: a célula pode ter medido o gerador", *e.Generator.CPUPctPeak))
	}
	if closed > 0 && chaos == nil {
		// Nothing should close during a cell of this etapa: an auction dying
		// mid-cell mixes contention with the closing edge in one number, and the
		// edge deserves a cell of its own (etapa 5). Under chaos the auctions
		// expiring mid-load are the point of the closerd scenario, so the number
		// goes to the evidence line below instead of to the warnings.
		warns = append(warns, fmt.Sprintf("closed=%d: ENDS_IN curto demais", closed))
	}

	evidence := fmt.Sprintf("replayed=%d in_flight=%d selecionadas=%d injetadas=%d",
		replayed, inFlight, selected, injected)
	if chaos != nil {
		evidence += fmt.Sprintf(" · caos=%s erro=%s closed=%d", chaos.Scenario, percent(rate), closed)
	}

	switch {
	case len(fails) > 0:
		f.Verdict = verdictFail
		f.Detail = strings.Join(append(append(fails, warns...), evidence), " · ")
	case len(warns) > 0:
		f.Verdict = verdictWarn
		f.Detail = strings.Join(append(warns, evidence), " · ")
	default:
		f.Detail = fmt.Sprintf("invalid=0 erro=%s gerador=%.0f%% do limite · %s",
			percent(rate), *e.Generator.CPUPctPeak, evidence)
	}
	return f
}

func percent(rate float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", rate*100), "0"), ".") + "%"
}
