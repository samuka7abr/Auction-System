package app_test

import (
	"strings"
	"testing"

	"github.com/samuka7abr/bid-storm/internal/app"
	"github.com/samuka7abr/bid-storm/internal/metrics"
)

func TestNewEngineBuildsTheImplementedStrategies(t *testing.T) {
	for _, strategy := range []string{app.StrategyOptimistic, app.StrategyPessimistic, app.StrategyShard} {
		t.Run(strategy, func(t *testing.T) {
			// No pool is dialled here: selecting an engine must not touch the
			// database, or the boot would fail for the wrong reason when Postgres
			// is slow to come up under the compose. Registering
			// lock_wait_duration_seconds is part of that — it happens against the
			// registry, never against the pool.
			engine, err := app.NewEngine(strategy, nil, metrics.NewRegistry())
			if err != nil {
				t.Fatalf("NewEngine: %v", err)
			}
			if engine == nil {
				t.Fatal("engine is nil with no error")
			}
		})
	}
}

// A strategy with no engine behind it has to fail at boot, naming the etapa it
// arrives in — not come up and answer 503 to a whole benchmark cell.
func TestNewEngineRefusesWhatDoesNotExistYet(t *testing.T) {
	tests := []struct {
		strategy string
		wantIn   string
	}{
		{"optimistc", "is not a strategy"},
		{"", "is not a strategy"},
	}

	for _, tc := range tests {
		t.Run(tc.strategy, func(t *testing.T) {
			engine, err := app.NewEngine(tc.strategy, nil, metrics.NewRegistry())
			if err == nil {
				t.Fatalf("NewEngine(%q) returned an engine, want an error", tc.strategy)
			}
			if engine != nil {
				t.Errorf("engine = %v, want nil alongside the error", engine)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not mention %q", err, tc.wantIn)
			}
		})
	}
}
