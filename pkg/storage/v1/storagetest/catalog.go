package storagetest

import (
	"context"
	"fmt"
	"slices"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// writeCase is one entry of a write catalog. The envelope, receipt and
// ownership cases run each entry against a fresh provider: setup prepares the
// records the write needs, call makes the write with the envelope under test
// in ctx and returns its result, and observe returns the state the write
// changes. Comparing observations shows that a refused write changed nothing
// and that a replay returned the original result without a second change.
type writeCase[P any] struct {
	method  string
	setup   func(t *testing.T, provider P)
	call    func(ctx context.Context, provider P) (any, error)
	observe func(t *testing.T, provider P) any
}

// writeOutcome is what one run of a write case produced.
type writeOutcome struct {
	result   any
	err      error
	observed any
}

// stateWriteCases and artifactWriteCases are the write catalogs. Every write
// method of a contract gains an entry as its provider obligations arrive.
var (
	stateWriteCases    []writeCase[storagev1.StateProvider]
	artifactWriteCases []writeCase[storagev1.ArtifactProvider]
)

// runWriteCase prepares provider for c, makes the write under envelope and
// observes the result.
func runWriteCase[P any](t *testing.T, provider P, c writeCase[P], envelope storagev1.WriteEnvelope) writeOutcome {
	t.Helper()
	if c.setup != nil {
		c.setup(t, provider)
	}
	result, err := c.call(storagev1.WithWriteEnvelope(t.Context(), envelope), provider)
	return writeOutcome{result: result, err: err, observed: c.observe(t, provider)}
}

// uncoveredWrites checks cases against the method catalog and returns, sorted,
// the write methods of contract that no case covers. A case for an unknown
// method, for a method that is not a write of contract, or for a method that
// another case already covers is an error.
func uncoveredWrites[P any](contract string, cases []writeCase[P]) ([]string, error) {
	covered := map[string]bool{}
	for _, c := range cases {
		info, ok := storagev1.LookupMethod(c.method)
		switch {
		case !ok:
			return nil, fmt.Errorf("write case %q names no contract method", c.method)
		case info.Kind != storagev1.MethodWrite || info.Contract != contract:
			return nil, fmt.Errorf("write case %q is not a %s write", c.method, contract)
		case covered[c.method]:
			return nil, fmt.Errorf("write case %q is listed twice", c.method)
		}
		covered[c.method] = true
	}
	var uncovered []string
	for name, info := range storagev1.Methods() {
		if info.Kind == storagev1.MethodWrite && info.Contract == contract && !covered[name] {
			uncovered = append(uncovered, name)
		}
	}
	slices.Sort(uncovered)
	return uncovered, nil
}
