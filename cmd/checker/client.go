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
}

func (c clientReport) required() map[string]*int64 {
	return map[string]*int64{
		"accepted": c.Accepted, "conflict": c.Conflict, "outbid": c.Outbid,
		"closed": c.Closed, "invalid": c.Invalid, "error": c.Error,
		"exhausted": c.Exhausted, "attempts": c.Attempts, "maxSeqSeen": c.MaxSeqSeen,
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
func checkDurability(db cellTotals, c clientReport) finding {
	f := finding{ID: "I5", Name: "durabilidade db x cliente", Verdict: verdictOK}
	accepted, maxSeq := *c.Accepted, *c.MaxSeqSeen

	switch {
	case db.Bids < accepted:
		f.Verdict = verdictFail
		f.Detail = fmt.Sprintf("LANCE CONFIRMADO SUMIU: db=%d cliente=%d", db.Bids, accepted)
	case db.Bids > accepted:
		f.Verdict = verdictFail
		f.Detail = fmt.Sprintf("BANCO À FRENTE: db=%d cliente=%d", db.Bids, accepted)
	default:
		f.Detail = fmt.Sprintf("db=%d cliente=%d", db.Bids, accepted)
	}

	// The watermark is global (emenda 28): the largest seq any VU saw confirmed,
	// in any auction. Together with I1's density it subsumes the per-auction
	// attribution — a durable 201 cannot vanish without either dropping the count
	// below the client's or opening a hole in some auction's sequence.
	if db.MaxSeq != maxSeq {
		f.Verdict = verdictFail
		f.Detail += fmt.Sprintf(" · watermark divergente: db=%d cliente=%d", db.MaxSeq, maxSeq)
	}
	return f
}

// The share of requests that may fail for real before the cell stops being a
// measurement. Same number as the k6 threshold, and for the same reason: above
// it, what broke was the measurement and not the engine.
const maxErrorRate = 0.01

// checkCellValidity is I6: it does not judge the engine, it judges whether this
// cell is worth reading at all.
func checkCellValidity(c clientReport, e envReport) finding {
	f := finding{ID: "I6", Name: "célula válida", Verdict: verdictOK}
	var fails, warns []string

	attempts, invalid, errs, closed := *c.Attempts, *c.Invalid, *c.Error, *c.Closed

	if attempts == 0 {
		fails = append(fails, "nenhuma tentativa registrada")
	}
	if invalid > 0 {
		// 400 expected_version_required. Decisão 21 put this series here for
		// free: above zero it means the k6 sent a bid without expectedVersion.
		fails = append(fails, fmt.Sprintf("invalid=%d: k6 mal configurado", invalid))
	}

	var rate float64
	if attempts > 0 {
		rate = float64(errs) / float64(attempts)
	}
	if rate >= maxErrorRate {
		fails = append(fails, fmt.Sprintf("erro=%s acima de %.0f%%: infra caindo", percent(rate), maxErrorRate*100))
	}
	if *e.Generator.Saturated {
		// A warning and not a failure: discarding the cell is the reader's call.
		// But never in silence — above 90% of its own limit the number may be the
		// generator's rather than auctiond's.
		warns = append(warns, fmt.Sprintf("gerador=%.0f%% do limite: a célula pode ter medido o gerador", *e.Generator.CPUPctPeak))
	}
	if closed > 0 {
		// Nothing should close during a cell of this etapa: an auction dying
		// mid-cell mixes contention with the closing edge in one number, and the
		// edge deserves a cell of its own (etapa 5).
		warns = append(warns, fmt.Sprintf("closed=%d: ENDS_IN curto demais", closed))
	}

	switch {
	case len(fails) > 0:
		f.Verdict = verdictFail
		f.Detail = strings.Join(append(fails, warns...), " · ")
	case len(warns) > 0:
		f.Verdict = verdictWarn
		f.Detail = strings.Join(warns, " · ")
	default:
		f.Detail = fmt.Sprintf("invalid=0 erro=%s gerador=%.0f%% do limite", percent(rate), *e.Generator.CPUPctPeak)
	}
	return f
}

func percent(rate float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", rate*100), "0"), ".") + "%"
}
