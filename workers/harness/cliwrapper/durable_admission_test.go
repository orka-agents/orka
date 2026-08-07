package cliwrapper

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/harness/ledger"
)

const durableAdmissionControllerToken = "controller-token"

func TestDurableAdmissionSurvivesWrapperRestart(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AuthValue = durableAdmissionControllerToken
	cfg.AdmissionLedgerPath = t.TempDir() + "/admission-ledger.db"
	cfg.Generic.Command = testEchoCommand
	request := validWrapperStartTurnRequest()

	baseURL, stop := startWrapperServerWithConfig(t, cfg, NewFakeAdapter(FakeBehaviorSuccess))
	client, err := harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn(first): %v", err)
	}
	_ = collectWrapperFrames(t, client, request.TurnID, 0)
	stop()

	baseURL, stop = startWrapperServerWithConfig(t, cfg, NewFakeAdapter(FakeBehaviorSuccess))
	defer stop()
	client, err = harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartTurn(context.Background(), request); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("StartTurn(after restart) error = %v, want durable conflict", err)
	}

	status, err := client.DurableTurnStatus(context.Background(), request.TurnID)
	if err != nil {
		t.Fatalf("DurableTurnStatus: %v", err)
	}
	if status.State != harness.DurableTurnTerminal || status.TerminalReceiptDigest == "" {
		t.Fatalf("durable status = %#v, want terminal receipt", status)
	}
}

func TestDurableTurnOutputSurvivesWrapperRestart(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AuthValue = durableAdmissionControllerToken
	cfg.AdmissionLedgerPath = filepath.Join(t.TempDir(), "admission-ledger.db")
	want := "result bytes retained across wrapper restart"
	cfg.Generic = GenericAdapterConfig{
		Command:    wrapperTestShellPath,
		Args:       []string{"-c", "printf '%s' '" + want + "' > result.txt"},
		PromptMode: PromptModeStdin,
		ResultMode: ResultModeFile,
		ResultFile: "result.txt",
	}
	request := validWrapperStartTurnRequest()

	baseURL, stop := startWrapperServerWithConfig(t, cfg, NewGenericAdapter(cfg.Generic))
	client, err := harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	last := frames[len(frames)-1]
	if last.Completed == nil || last.Completed.OutputRef != localOutputRef {
		t.Fatalf("terminal frame = %#v, want durable local output ref", last)
	}
	stop()

	baseURL, stop = startWrapperServerWithConfig(t, cfg, NewGenericAdapter(cfg.Generic))
	client, err = harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.FetchTurnOutput(context.Background(), request.TurnID, last.Completed.OutputRef)
	if err != nil {
		t.Fatalf("FetchTurnOutput(after restart): %v", err)
	}
	if string(got) != want {
		t.Fatalf("fetched output = %q, want %q", got, want)
	}
	receipt, err := harness.DurableTurnTerminalReceiptFromFrame(last)
	if err != nil {
		t.Fatalf("terminal receipt: %v", err)
	}
	receiptDigest, err := harness.DurableTurnTerminalReceiptDigest(receipt)
	if err != nil {
		t.Fatalf("terminal receipt digest: %v", err)
	}
	acknowledgement := harness.TurnOutputAcknowledgementRequest{
		Version:               harness.ProtocolVersion,
		TurnID:                request.TurnID,
		OutputRef:             last.Completed.OutputRef,
		TerminalReceiptDigest: receiptDigest,
	}
	if err := client.AcknowledgeTurnOutput(context.Background(), acknowledgement); err != nil {
		t.Fatalf("AcknowledgeTurnOutput: %v", err)
	}
	if _, err := client.FetchTurnOutput(
		context.Background(), request.TurnID, last.Completed.OutputRef,
	); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("FetchTurnOutput(after acknowledgement) error = %v, want 404", err)
	}
	stop()

	baseURL, stop = startWrapperServerWithConfig(t, cfg, NewGenericAdapter(cfg.Generic))
	defer stop()
	client, err = harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AcknowledgeTurnOutput(context.Background(), acknowledgement); err != nil {
		t.Fatalf("AcknowledgeTurnOutput(after restart): %v", err)
	}
}

func TestDurableTurnEnvValuesAreRedactedAcrossRestart(t *testing.T) {
	testCases := []struct {
		name   string
		script string
		failed bool
	}{
		{
			name: "completed",
			script: "printf 'stdout:%s' \"$OPAQUE_TURN_ENV\"; " +
				"printf 'stderr:%s' \"$OPAQUE_TURN_ENV\" >&2; " +
				"printf 'result:%s' \"$OPAQUE_TURN_ENV\" > result.txt",
		},
		{
			name: "failed",
			script: "printf 'stdout:%s' \"$OPAQUE_TURN_ENV\"; " +
				"printf 'stderr:%s' \"$OPAQUE_TURN_ENV\" >&2; " +
				"printf 'result:%s' \"$OPAQUE_TURN_ENV\" > result.txt; exit 7",
			failed: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			const opaqueValue = "opaque-turn-env-value-84e23a"
			cfg := DefaultConfig()
			cfg.AuthValue = durableAdmissionControllerToken
			cfg.AdmissionLedgerPath = filepath.Join(t.TempDir(), "admission-ledger.db")
			cfg.Generic = GenericAdapterConfig{
				Command:    wrapperTestShellPath,
				Args:       []string{"-c", testCase.script},
				PromptMode: PromptModeStdin,
				ResultMode: ResultModeFile,
				ResultFile: "result.txt",
			}
			request := validWrapperStartTurnRequest()
			request.Input.Env = []harness.TurnEnvVar{{Name: "OPAQUE_TURN_ENV", Value: opaqueValue}}
			request = sealDurableWrapperRequest(request)

			baseURL, stop := startWrapperServerWithConfig(t, cfg, NewGenericAdapter(cfg.Generic))
			client, err := harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.StartTurn(context.Background(), request); err != nil {
				t.Fatalf("StartTurn: %v", err)
			}
			frames := collectWrapperFrames(t, client, request.TurnID, 0)
			encoded, err := json.Marshal(frames)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), opaqueValue) {
				t.Fatalf("SSE frames leaked exact turn env value: %s", encoded)
			}
			if !strings.Contains(string(encoded), "[REDACTED]") {
				t.Fatalf("SSE frames did not contain a redaction marker: %s", encoded)
			}

			last := frames[len(frames)-1]
			var outputRef string
			if testCase.failed {
				if last.Failed == nil {
					t.Fatalf("terminal frame = %#v, want failed", last)
				}
				outputRef = last.Failed.OutputRef
			} else {
				if last.Completed == nil {
					t.Fatalf("terminal frame = %#v, want completed", last)
				}
				outputRef = last.Completed.OutputRef
			}
			if outputRef != localOutputRef {
				t.Fatalf("terminal output ref = %q, want %q", outputRef, localOutputRef)
			}
			stop()

			baseURL, stop = startWrapperServerWithConfig(t, cfg, NewGenericAdapter(cfg.Generic))
			defer stop()
			client, err = harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
			if err != nil {
				t.Fatal(err)
			}
			output, err := client.FetchTurnOutput(context.Background(), request.TurnID, outputRef)
			if err != nil {
				t.Fatalf("FetchTurnOutput(after restart): %v", err)
			}
			if strings.Contains(string(output), opaqueValue) {
				t.Fatalf("durable output leaked exact turn env value: %q", output)
			}
			if !strings.Contains(string(output), "[REDACTED]") {
				t.Fatalf("durable output did not contain a redaction marker: %q", output)
			}
		})
	}
}

func TestDurableAdmissionReconcilesOrphansOnWrapperRestart(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.AuthValue = durableAdmissionControllerToken
	cfg.AdmissionLedgerPath = filepath.Join(t.TempDir(), "admission-ledger.db")
	cfg.Generic.Command = testEchoCommand
	digest := "sha256:" + strings.Repeat("a", 64)

	l, err := ledger.Open(cfg.AdmissionLedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.AdmitTurn(
		ctx, "admitted-orphan", "task-uid", 1, digest, "runtime-admitted", "correlation-admitted",
	); err != nil {
		t.Fatalf("admit orphan: %v", err)
	}
	if _, _, err := l.AdmitTurn(
		ctx, "accepted-orphan", "task-uid", 2, digest, "runtime-accepted", "correlation-accepted",
	); err != nil {
		t.Fatalf("admit accepted orphan: %v", err)
	}
	if err := l.MarkTurnAccepted(ctx, "accepted-orphan"); err != nil {
		t.Fatalf("accept orphan: %v", err)
	}
	if err := l.CloseAdmission(ctx); err != nil {
		t.Fatalf("close admission: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close seed ledger: %v", err)
	}

	baseURL, stop := startWrapperServerWithConfig(t, cfg, NewFakeAdapter(FakeBehaviorSuccess))
	defer stop()
	client, err := harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := client.DurableTurnStatus(ctx, "admitted-orphan")
	if err != nil {
		t.Fatalf("DurableTurnStatus(admitted orphan): %v", err)
	}
	if admitted.State != harness.DurableTurnRejected {
		t.Fatalf("admitted orphan state = %s, want Rejected", admitted.State)
	}
	accepted, err := client.DurableTurnStatus(ctx, "accepted-orphan")
	if err != nil {
		t.Fatalf("DurableTurnStatus(accepted orphan): %v", err)
	}
	if accepted.State != harness.DurableTurnOutcomeUnknown || accepted.TerminalReceipt == nil ||
		accepted.TerminalReceipt.Kind != harness.DurableTurnTerminalOutcomeUnknown ||
		accepted.TerminalReceipt.RuntimeSessionID != "runtime-accepted" ||
		accepted.TerminalReceipt.CorrelationID != "correlation-accepted" {
		t.Fatalf("accepted orphan status = %#v, want exact OutcomeUnknown identity", accepted)
	}
	drain, err := client.DurableDrainStatus(ctx)
	if err != nil {
		t.Fatalf("DurableDrainStatus: %v", err)
	}
	if !drain.AdmissionClosed || !drain.Completed || len(drain.Unsettled) != 0 {
		t.Fatalf("drain after orphan reconciliation = %#v, want completed", drain)
	}
}

func TestDurableRolloverReopensOnlyPreparedReplacementGeneration(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.AuthValue = durableAdmissionControllerToken
	cfg.AdmissionLedgerPath = filepath.Join(t.TempDir(), "admission-ledger.db")
	cfg.Generic.Command = testEchoCommand
	request := validWrapperStartTurnRequest()

	baseURL, stop := startWrapperServerWithConfig(t, cfg, NewFakeAdapter(FakeBehaviorSuccess))
	client, err := harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartTurn(ctx, request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	_ = collectWrapperFrames(t, client, request.TurnID, 0)
	if _, err := client.CloseDurableAdmission(ctx); err != nil {
		t.Fatalf("CloseDurableAdmission: %v", err)
	}
	if status, err := client.DurableDrainStatus(ctx); err != nil || !status.Completed {
		t.Fatalf("DurableDrainStatus() = %#v, %v, want completed", status, err)
	}
	prepared, err := client.PrepareDurableRollover(ctx, "2")
	if err != nil {
		t.Fatalf("PrepareDurableRollover: %v", err)
	}
	if !prepared.Prepared || prepared.CurrentGeneration != "1" || prepared.NextGeneration != "2" {
		t.Fatalf("rollover preparation = %#v", prepared)
	}
	stop()

	cfg.LedgerGeneration = "2"
	baseURL, stop = startWrapperServerWithConfig(t, cfg, NewFakeAdapter(FakeBehaviorSuccess))
	defer stop()
	client, err = harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
	if err != nil {
		t.Fatal(err)
	}
	drain, err := client.DurableDrainStatus(ctx)
	if err != nil {
		t.Fatalf("DurableDrainStatus(after rollover): %v", err)
	}
	if drain.AdmissionClosed || drain.Completed {
		t.Fatalf("replacement drain status = %#v, want admission reopened", drain)
	}
	next := validWrapperStartTurnRequest()
	next.TurnID = "turn-after-rollover"
	next.CorrelationID = "correlation-after-rollover"
	next = sealDurableWrapperRequest(next)
	if _, err := client.StartTurn(ctx, next); err != nil {
		t.Fatalf("StartTurn(after rollover): %v", err)
	}
	_ = collectWrapperFrames(t, client, next.TurnID, 0)
	status, err := client.DurableTurnStatus(ctx, request.TurnID)
	if err != nil || status.State != harness.DurableTurnTerminal {
		t.Fatalf("pre-rollover tombstone = %#v, %v", status, err)
	}
}

func TestDurableRolloverAbortReopensOnlyExactCurrentGeneration(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.AuthValue = durableAdmissionControllerToken
	cfg.AdmissionLedgerPath = filepath.Join(t.TempDir(), "admission-ledger.db")
	cfg.Generic.Command = testEchoCommand

	baseURL, stop := startWrapperServerWithConfig(t, cfg, NewFakeAdapter(FakeBehaviorSuccess))
	defer stop()
	client, err := harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CloseDurableAdmission(ctx); err != nil {
		t.Fatalf("CloseDurableAdmission: %v", err)
	}
	if _, err := client.PrepareDurableRollover(ctx, "2"); err != nil {
		t.Fatalf("PrepareDurableRollover: %v", err)
	}

	unauthenticated, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unauthenticated.AbortDurableRollover(ctx, "1"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthenticated AbortDurableRollover error = %v, want 401", err)
	}
	if _, err := client.AbortDurableRollover(ctx, "2"); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("AbortDurableRollover(stale generation) error = %v, want 409", err)
	}
	if status, err := client.DurableDrainStatus(ctx); err != nil || !status.AdmissionClosed || !status.Completed {
		t.Fatalf("DurableDrainStatus(after stale abort) = %#v, %v, want completed close", status, err)
	}

	aborted, err := client.AbortDurableRollover(ctx, "1")
	if err != nil {
		t.Fatalf("AbortDurableRollover: %v", err)
	}
	if !aborted.AdmissionReopened || aborted.CurrentGeneration != "1" {
		t.Fatalf("rollover abort = %#v", aborted)
	}
	if _, err := client.AbortDurableRollover(ctx, "1"); err != nil {
		t.Fatalf("AbortDurableRollover(idempotent): %v", err)
	}
	if status, err := client.DurableDrainStatus(ctx); err != nil || status.AdmissionClosed || status.Completed {
		t.Fatalf("DurableDrainStatus(after abort) = %#v, %v, want open", status, err)
	}

	request := validWrapperStartTurnRequest()
	request.TurnID = "turn-after-aborted-rollover"
	request.CorrelationID = "correlation-after-aborted-rollover"
	request = sealDurableWrapperRequest(request)
	if _, err := client.StartTurn(ctx, request); err != nil {
		t.Fatalf("StartTurn(after abort): %v", err)
	}
	_ = collectWrapperFrames(t, client, request.TurnID, 0)
}

func TestDurableAdmissionRejectsRequestDigestMismatch(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AuthValue = durableAdmissionControllerToken
	cfg.AdmissionLedgerPath = t.TempDir() + "/admission-ledger.db"
	cfg.Generic.Command = testEchoCommand
	baseURL, stop := startWrapperServerWithConfig(t, cfg, NewFakeAdapter(FakeBehaviorSuccess))
	defer stop()
	client, err := harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	request.Input.Prompt = "mutated after request digest"
	if _, err := client.StartTurn(context.Background(), request); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("StartTurn(mutated) error = %v, want digest rejection", err)
	}
}

//nolint:gocyclo // One end-to-end test keeps the authenticated close/drain lifecycle in protocol order.
func TestDurableAdmissionCloseAndDrain(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AuthValue = durableAdmissionControllerToken
	cfg.AdmissionLedgerPath = t.TempDir() + "/admission-ledger.db"
	cfg.Generic.Command = testEchoCommand
	baseURL, stop := startWrapperServerWithConfig(t, cfg, NewFakeAdapter(FakeBehaviorCancellation))
	defer stop()

	unauthenticated, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unauthenticated.DurableTurnStatus(context.Background(), "unauthenticated-turn"); err == nil ||
		!strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthenticated DurableTurnStatus error = %v, want 401", err)
	}
	if _, err := unauthenticated.CloseDurableAdmission(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthenticated CloseDurableAdmission error = %v, want 401", err)
	}
	if _, err := unauthenticated.DurableDrainStatus(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthenticated DurableDrainStatus error = %v, want 401", err)
	}

	client, err := harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	openDrain, err := client.DurableDrainStatus(context.Background())
	if err != nil {
		t.Fatalf("DurableDrainStatus(before close): %v", err)
	}
	if openDrain.AdmissionClosed || openDrain.Completed || len(openDrain.Unsettled) != 1 ||
		openDrain.Unsettled[0].TurnID != string(request.TurnID) ||
		openDrain.Unsettled[0].State != harness.DurableTurnAccepted {
		t.Fatalf("drain before close = %#v, want one accepted turn with admission open", openDrain)
	}
	if _, err := client.CloseDurableAdmission(context.Background()); err != nil {
		t.Fatalf("CloseDurableAdmission: %v", err)
	}
	if _, err := client.CloseDurableAdmission(context.Background()); err != nil {
		t.Fatalf("CloseDurableAdmission(idempotent replay): %v", err)
	}
	late := validWrapperStartTurnRequest()
	late.TurnID = "turn-after-close"
	late.CorrelationID = "corr-after-close"
	late = sealDurableWrapperRequest(late)
	if _, err := client.StartTurn(context.Background(), late); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("StartTurn(after close) error = %v, want admission closed", err)
	}
	drain, err := client.DurableDrainStatus(context.Background())
	if err != nil {
		t.Fatalf("DurableDrainStatus: %v", err)
	}
	if !drain.AdmissionClosed || drain.AdmissionClosedAt.IsZero() || drain.Completed ||
		len(drain.Unsettled) != 1 || drain.Unsettled[0].TurnID != string(request.TurnID) {
		t.Fatalf("drain = %#v, want closed with one unsettled accepted turn", drain)
	}

	cancelDurableAdmissionTurn(t, client, request)
	_ = collectWrapperFrames(t, client, request.TurnID, 0)
	eventually(t, 2*time.Second, func() bool {
		settled, statusErr := client.DurableDrainStatus(context.Background())
		return statusErr == nil && settled.AdmissionClosed && settled.Completed && len(settled.Unsettled) == 0
	})
	settled, err := client.DurableDrainStatus(context.Background())
	if err != nil {
		t.Fatalf("DurableDrainStatus(after settlement): %v", err)
	}
	if !settled.AdmissionClosed || !settled.Completed || len(settled.Unsettled) != 0 {
		t.Fatalf("settled drain = %#v, want closed and complete", settled)
	}
}

func TestDurableAdmissionPersistsCapacityRejection(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AuthValue = durableAdmissionControllerToken
	cfg.AdmissionLedgerPath = filepath.Join(t.TempDir(), "admission-ledger.db")
	cfg.Generic.Command = testEchoCommand
	baseURL, stop := startWrapperServerWithConfig(t, cfg, NewFakeAdapter(FakeBehaviorCancellation))
	defer stop()
	client, err := harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
	if err != nil {
		t.Fatal(err)
	}

	first := validWrapperStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), first); err != nil {
		t.Fatalf("StartTurn(first): %v", err)
	}
	second := validWrapperStartTurnRequest()
	second.TurnID = "turn-capacity-rejected"
	second.CorrelationID = "corr-capacity-rejected"
	second = sealDurableWrapperRequest(second)
	if _, err := client.StartTurn(context.Background(), second); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("StartTurn(second) error = %v, want capacity conflict", err)
	}
	status, err := client.DurableTurnStatus(context.Background(), second.TurnID)
	if err != nil {
		t.Fatalf("DurableTurnStatus(rejected): %v", err)
	}
	if status.State != harness.DurableTurnRejected ||
		status.TaskUID != second.Metadata[harness.MetadataTaskUID] || status.Attempt != 1 ||
		status.RequestDigest != second.Metadata[harness.MetadataRequestDigest] ||
		status.TerminalReceipt != nil || status.TerminalReceiptDigest != "" {
		t.Fatalf("durable rejected status = %#v, want sealed non-acceptance proof", status)
	}

	cancelDurableAdmissionTurn(t, client, first)
	_ = collectWrapperFrames(t, client, first.TurnID, 0)
}

func TestDurableAdmissionAdminEndpointsFailWithoutLedger(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.Generic.Command = testEchoCommand
	baseURL, stop := startWrapperServerWithConfig(t, cfg, NewFakeAdapter(FakeBehaviorSuccess))
	defer stop()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.DurableTurnStatus(context.Background(), "missing-ledger-turn"); err == nil ||
		!strings.Contains(err.Error(), "503") {
		t.Fatalf("DurableTurnStatus without ledger error = %v, want 503", err)
	}
	if _, err := client.CloseDurableAdmission(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "503") {
		t.Fatalf("CloseDurableAdmission without ledger error = %v, want 503", err)
	}
	if _, err := client.DurableDrainStatus(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "503") {
		t.Fatalf("DurableDrainStatus without ledger error = %v, want 503", err)
	}
}

func TestDurableAdmissionRejectsUnopenableLedgerPath(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AuthValue = durableAdmissionControllerToken
	cfg.AdmissionLedgerPath = filepath.Join(t.TempDir(), "missing-parent", "admission-ledger.db")
	cfg.Generic.Command = testEchoCommand
	server, err := NewServer(cfg, NewFakeAdapter(FakeBehaviorSuccess))
	if err == nil {
		_ = server.Close()
		t.Fatal("NewServer with unopenable admission ledger path succeeded")
	}
}

func TestDurableAdmissionPreservesIdentityAcrossAcceptedAndTerminalStatus(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AuthValue = durableAdmissionControllerToken
	cfg.AdmissionLedgerPath = filepath.Join(t.TempDir(), "admission-ledger.db")
	cfg.Generic.Command = testEchoCommand
	baseURL, stop := startWrapperServerWithConfig(t, cfg, NewFakeAdapter(FakeBehaviorCancellation))
	defer stop()
	client, err := harness.NewClient(baseURL, harness.WithBearerToken(cfg.AuthValue))
	if err != nil {
		t.Fatal(err)
	}

	request := validWrapperStartTurnRequest()
	request.Metadata[harness.MetadataAttempt] = "3"
	request = sealDurableWrapperRequest(request)
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	accepted, err := client.DurableTurnStatus(context.Background(), request.TurnID)
	if err != nil {
		t.Fatalf("DurableTurnStatus(accepted): %v", err)
	}
	assertDurableAdmissionIdentity(t, accepted, request, harness.DurableTurnAccepted)
	if accepted.TerminalReceipt != nil || accepted.TerminalReceiptDigest != "" {
		t.Fatalf("accepted status contains terminal receipt: %#v", accepted)
	}

	cancelDurableAdmissionTurn(t, client, request)
	_ = collectWrapperFrames(t, client, request.TurnID, 0)
	terminal, err := client.DurableTurnStatus(context.Background(), request.TurnID)
	if err != nil {
		t.Fatalf("DurableTurnStatus(terminal): %v", err)
	}
	assertDurableAdmissionIdentity(t, terminal, request, harness.DurableTurnTerminal)
	if terminal.TerminalReceipt == nil || terminal.TerminalReceiptDigest == "" ||
		terminal.TerminalReceipt.TurnID != request.TurnID ||
		terminal.TerminalReceipt.Kind != harness.DurableTurnTerminalCancelled {
		t.Fatalf("terminal status = %#v, want canonical cancellation receipt", terminal)
	}
}

func cancelDurableAdmissionTurn(t *testing.T, client *harness.Client, request harness.StartTurnRequest) {
	t.Helper()
	if _, err := client.CancelTurn(context.Background(), harness.CancelTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Reason:           "test cleanup",
	}); err != nil {
		t.Fatalf("CancelTurn: %v", err)
	}
}

func assertDurableAdmissionIdentity(
	t *testing.T,
	status *harness.DurableTurnStatus,
	request harness.StartTurnRequest,
	wantState harness.DurableTurnAdmissionState,
) {
	t.Helper()
	if status.State != wantState || status.TurnID != string(request.TurnID) ||
		status.TaskUID != request.Metadata[harness.MetadataTaskUID] || status.Attempt != 3 ||
		status.RequestDigest != request.Metadata[harness.MetadataRequestDigest] || status.UpdatedAt.IsZero() {
		t.Fatalf("durable status = %#v, want state %s and sealed request identity", status, wantState)
	}
}
