/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/cli/client"
	oversightv1 "github.com/orka-agents/orka/pkg/oversight/v1"
)

func replayLine(t *testing.T, id string, observed time.Time) string {
	t.Helper()
	raw, err := json.Marshal(oversightv1.OversightDecision{
		ID: id, TaskName: "task", ObservedAt: observed, Mode: oversightv1.ModeAudit,
		Disposition: oversightv1.DispositionAllow, EvidenceLevel: oversightv1.EvidenceLevelObserved,
		Coverage: oversightv1.ActionCoverage{StreamState: "ok", SensorState: "events"},
		Action:   oversightv1.ActionEnvelope{ToolName: "code_exec"},
	})
	require.NoError(t, err)
	return string(raw)
}

type fakeIngest struct {
	mu      sync.Mutex
	stored  map[string]bool
	batches [][]oversightv1.OversightDecision
	paths   []string
}

func (f *fakeIngest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req oversightv1.IngestRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.paths = append(f.paths, r.URL.Path)
	f.batches = append(f.batches, req.Decisions)
	var response oversightv1.IngestResponse
	for i, d := range req.Decisions {
		duplicate := f.stored[d.ID]
		f.stored[d.ID] = true
		if duplicate {
			response.Duplicates++
		} else {
			response.Ingested++
		}
		response.Results = append(response.Results, oversightv1.IngestResult{ID: d.ID, Seq: int64(i + 1), Duplicate: duplicate})
	}
	_ = json.NewEncoder(w).Encode(response)
}

func TestOversightReplayBatchesShiftsAndIsIdempotent(t *testing.T) {
	ingest := &fakeIngest{stored: map[string]bool{}}
	server := httptest.NewServer(ingest)
	defer server.Close()
	start := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	lines := make([]string, 0, 5)
	for i := range 5 {
		lines = append(lines, replayLine(t, fmt.Sprintf("task@%d", i), start.Add(time.Duration(i)*time.Minute)))
	}
	file := filepath.Join(t.TempDir(), "decisions.jsonl")
	require.NoError(t, os.WriteFile(file, []byte("# recorded run\n"+strings.Join(lines, "\n")+"\n\n"), 0o600))
	c := client.NewWithNamespace(server.URL, "", "team")
	now := func() time.Time { return start.Add(48 * time.Hour) }

	var out bytes.Buffer
	opts := replayOptions{files: []string{file}, namespace: "team", batchSize: 2, shiftToNow: true, runID: "r1"}
	require.NoError(t, runOversightReplay(t.Context(), c, opts, &out, now))
	require.Contains(t, out.String(), "5 ingested, 0 already stored")
	require.Len(t, ingest.batches, 3)
	require.Equal(t, "/internal/v1/oversight/team/decisions", ingest.paths[0])
	last := ingest.batches[2][0]
	require.True(t, last.ObservedAt.Equal(now()), "the newest record lands at now")
	first := ingest.batches[0][0]
	require.Equal(t, 4*time.Minute, last.ObservedAt.Sub(first.ObservedAt), "spacing is kept")
	require.Equal(t, "r1", first.RunID)

	out.Reset()
	require.NoError(t, runOversightReplay(t.Context(), c, opts, &out, now))
	require.Contains(t, out.String(), "0 ingested, 5 already stored")
}

func TestOversightReplaySendsNothingWhenALineIsInvalid(t *testing.T) {
	ingest := &fakeIngest{stored: map[string]bool{}}
	server := httptest.NewServer(ingest)
	defer server.Close()
	file := filepath.Join(t.TempDir(), "decisions.jsonl")
	good := replayLine(t, "task@0", time.Now())
	require.NoError(t, os.WriteFile(file, []byte(good+"\n{not json\n"+`{"id":"x","surprise":1}`+"\n"), 0o600))
	c := client.NewWithNamespace(server.URL, "", "team")
	err := runOversightReplay(t.Context(), c, replayOptions{files: []string{file}, namespace: "team", batchSize: 10}, &bytes.Buffer{}, time.Now)
	require.ErrorContains(t, err, "decisions.jsonl:2:")
	require.ErrorContains(t, err, "decisions.jsonl:3:")
	require.Empty(t, ingest.batches)

	err = runOversightReplay(t.Context(), c, replayOptions{files: []string{file}, batchSize: 10}, &bytes.Buffer{}, time.Now)
	require.ErrorContains(t, err, "namespace is required")
}

func TestOversightReplayPlaysATakeWithItsTimingAsNewDecisions(t *testing.T) {
	ingest := &fakeIngest{stored: map[string]bool{}}
	server := httptest.NewServer(ingest)
	defer server.Close()
	start := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	first := replayLine(t, "task@0", start)
	var second oversightv1.OversightDecision
	require.NoError(t, json.Unmarshal([]byte(replayLine(t, "task@1", start.Add(20*time.Second))), &second))
	second.Action.PriorActionIDs = []string{"task@0"}
	raw, _ := json.Marshal(second)
	file := filepath.Join(t.TempDir(), "take.jsonl")
	require.NoError(t, os.WriteFile(file, []byte(first+"\n"+string(raw)+"\n"), 0o600))
	c := client.NewWithNamespace(server.URL, "", "team")
	clock := start.Add(time.Hour)
	now := func() time.Time { clock = clock.Add(time.Second); return clock }

	began := time.Now()
	var out bytes.Buffer
	opts := replayOptions{files: []string{file}, namespace: "team", batchSize: 50, preserveTiming: true,
		speed: 100, maxGap: time.Second, idSuffix: "-t2"}
	require.NoError(t, runOversightReplay(t.Context(), c, opts, &out, now))
	require.GreaterOrEqual(t, time.Since(began), 150*time.Millisecond, "the recorded 20s gap plays at 100x")
	require.Len(t, ingest.batches, 2, "the decisions were due apart")
	played := ingest.batches[1][0]
	require.Equal(t, "task-t2@1", played.ID)
	require.Equal(t, "task-t2", played.TaskName)
	require.Equal(t, []string{"task-t2@0"}, played.Action.PriorActionIDs)
	require.True(t, played.ObservedAt.After(start.Add(time.Hour)), "stamped with the time it was sent")
	require.Contains(t, out.String(), "Played 2 decisions")
}

func TestOversightReplayWithTimingSendsDecisionsThatAreDueTogether(t *testing.T) {
	ingest := &fakeIngest{stored: map[string]bool{}}
	server := httptest.NewServer(ingest)
	defer server.Close()
	start := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	lines := []string{
		replayLine(t, "a@0", start), replayLine(t, "b@0", start), replayLine(t, "c@0", start),
		replayLine(t, "d@0", start.Add(10*time.Second)),
	}
	file := filepath.Join(t.TempDir(), "take.jsonl")
	require.NoError(t, os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	c := client.NewWithNamespace(server.URL, "", "team")
	opts := replayOptions{files: []string{file}, namespace: "team", batchSize: 2, preserveTiming: true,
		speed: 100, maxGap: time.Second}
	require.NoError(t, runOversightReplay(t.Context(), c, opts, &bytes.Buffer{}, time.Now))
	sizes := make([]int, 0, len(ingest.batches))
	for _, batch := range ingest.batches {
		sizes = append(sizes, len(batch))
	}
	require.Equal(t, []int{2, 1, 1}, sizes, "three due at once are split by --batch-size; the fourth is due later")
}
