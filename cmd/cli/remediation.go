package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/orka-agents/orka/internal/remediation/contract"
)

func newRemediationCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "remediate",
		Short: "Reproduce a supplied report, make one fix, and independently verify it",
		Args:  remediationArgs(0),
		Long: "Submit one report to the operator-configured Orka remediation service. " +
			"The service uses separate agent stages; failed candidates are not automatically retried or published. " +
			"The default backend provides source-harness evidence. Application-runtime evidence requires explicit operator qualification.",
		RunE: func(*cobra.Command, []string) error {
			return errors.New("select a remediation action: start, status, cancel, or download")
		},
	}
	command.PersistentFlags().String("token-file", "", "private Kubernetes bearer-token file (otherwise use saved login)")
	command.SetFlagErrorFunc(func(*cobra.Command, error) error {
		return errors.New("invalid remediation flags; see --help")
	})
	command.AddCommand(newRemediationStartCmd(), newRemediationStatusCmd(), newRemediationCancelCmd(), newRemediationDownloadCmd())
	return command
}

func newRemediationStartCmd() *cobra.Command {
	var input, requestID, mode, requiredEvidenceScope string
	var wait bool
	var timeout time.Duration
	command := &cobra.Command{
		Use:   "start",
		Short: "Submit a JSON report with explicit repository and ref",
		Args:  remediationArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if input == "" {
				return errors.New("--input is required")
			}
			if timeout <= 0 || timeout > 2*time.Hour {
				return errors.New("--timeout must be positive and at most two hours")
			}
			if cmd.Flags().Changed("required-evidence-scope") && !contract.EvidenceScope(requiredEvidenceScope).Valid() {
				return errors.New("--required-evidence-scope must be source-harness or application-runtime")
			}
			request, err := readRemediationInput(input, contract.Mode(mode), requestID)
			if err != nil {
				return err
			}
			request.RequiredEvidenceScope = contract.EvidenceScope(requiredEvidenceScope)
			api, err := remediationAPI(cmd)
			if err != nil {
				return err
			}
			if request.RequestID == "" {
				var identity [16]byte
				if _, err := rand.Read(identity[:]); err != nil {
					return errors.New("could not create a request ID")
				}
				request.RequestID = "rem-" + hex.EncodeToString(identity[:])
			}
			progress := cmd.OutOrStdout()
			if wait {
				progress = cmd.ErrOrStderr()
			}
			// Print the durable lookup key before any request can have effects.
			// An interrupted response must not lead to a fresh creation identity.
			if err := printRemediationStatusTo(progress, &contract.Status{ID: request.RequestID}); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			status, err := api.statusRequest(ctx, http.MethodPost, "", request.RequestID, &request)
			if err != nil {
				return fmt.Errorf("%w; check status with the printed requestID before retrying the identical request with that same ID", err)
			}
			if err := printRemediationStatusTo(progress, status); err != nil {
				return err
			}
			if wait {
				status, err = waitRemediation(ctx, cmd, api, status, 2*time.Second)
				if err != nil {
					return err
				}
				return errors.Join(remediationCompletion(status, request.Mode), printRemediationTaskResult(ctx, cmd, api, status))
			}
			if contract.TerminalPhase(status.Phase) {
				return remediationCompletion(status, request.Mode)
			}
			return nil
		},
	}
	command.Flags().StringVar(&input, "input", "", "report JSON: title, problem or description, repository, and ref (required)")
	command.Flags().StringVar(&mode, "mode", string(contract.Generate), "generate or validate (report only)")
	command.Flags().StringVar(&requiredEvidenceScope, "required-evidence-scope", "", "source-harness (default) or application-runtime (strict; requires an operator-qualified application backend)")
	command.Flags().StringVar(&requestID, "request-id", "", "stable request ID; reuse only with the identical request")
	command.Flags().BoolVar(&wait, "wait", false, "wait and print the normal parent Task result; progress goes to stderr, without resubmitting")
	command.Flags().DurationVar(&timeout, "timeout", 45*time.Minute, "command deadline; expiry does not cancel the remote run")
	return command
}

func newRemediationStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use: "status RUN_ID", Short: "Read the exact remote run's status", Args: remediationArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := remediationAPI(cmd)
			if err != nil {
				return err
			}
			status, err := api.statusRequest(cmd.Context(), http.MethodGet, "/"+args[0], args[0], nil)
			if err != nil {
				return err
			}
			return printRemediationStatus(cmd, status)
		},
	}
}

func newRemediationCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use: "cancel RUN_ID", Short: "Request cancellation of the exact remote run", Args: remediationArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := remediationAPI(cmd)
			if err != nil {
				return err
			}
			status, err := api.statusRequest(cmd.Context(), http.MethodPost, "/"+args[0]+"/cancel", args[0], nil)
			if err != nil {
				return err
			}
			return printRemediationStatus(cmd, status)
		},
	}
}

func newRemediationDownloadCmd() *cobra.Command {
	var output string
	command := &cobra.Command{
		Use: "download RUN_ID", Short: "Save a candidate patch to a new private file", Args: remediationArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				return errors.New("--output is required; patches are never printed to the terminal")
			}
			if err := remediationOutputAvailable(output); err != nil {
				return err
			}
			api, err := remediationAPI(cmd)
			if err != nil {
				return err
			}
			status, err := api.statusRequest(cmd.Context(), http.MethodGet, "/"+args[0], args[0], nil)
			if err != nil {
				return err
			}
			if !status.PatchReady {
				return errors.New("no candidate patch is available for this run")
			}
			patch, err := api.request(cmd.Context(), http.MethodGet, "/"+args[0]+"/patch", nil, remediationMaxPatchBytes, remediationPatchMIME)
			if err != nil {
				return err
			}
			if err := writeRemediationPatch(output, patch); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.ErrOrStderr(), remediationPatchLabel(status)); err != nil {
				return errors.New("could not print the patch verification label")
			}
			return printRemediationStatus(cmd, status)
		},
	}
	command.Flags().StringVar(&output, "output", "", "new patch file, created owner-only; existing files, symlinks, and '..' components are refused")
	return command
}

func remediationPatchLabel(status *contract.Status) string {
	if status.Phase != contract.PhaseSucceeded || status.Outcome != contract.OutcomeVerified {
		return "Candidate patch saved (not verified)."
	}
	switch status.EvidenceScope {
	case contract.SourceHarness:
		return "Patch saved (verified source-harness checks only; not application-runtime evidence)."
	case contract.ApplicationRuntime:
		return "Patch saved (verified application-runtime checks only)."
	default:
		return "Patch saved (verified checks; evidence scope unrecorded)."
	}
}

func remediationArgs(count int) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != count {
			return errors.New("invalid remediation arguments; see --help")
		}
		if count == 1 && !validRemediationID(args[0]) {
			return errors.New("run ID must be a canonical DNS subdomain of at most 63 characters")
		}
		return nil
	}
}

func printRemediationStatus(cmd *cobra.Command, status *contract.Status) error {
	return printRemediationStatusTo(cmd.OutOrStdout(), status)
}

func printRemediationStatusTo(writer io.Writer, status *contract.Status) error {
	if !validRemediationFeedback(status) {
		return errors.New("server returned invalid remediation outcome metadata")
	}
	if err := json.NewEncoder(writer).Encode(struct {
		RequestID     string                 `json:"requestID"`
		Phase         string                 `json:"phase,omitempty"`
		Stage         string                 `json:"stage,omitempty"`
		Outcome       string                 `json:"outcome,omitempty"`
		Reason        string                 `json:"reason,omitempty"`
		TaskName      string                 `json:"taskName,omitempty"`
		TaskUID       string                 `json:"taskUID,omitempty"`
		EvidenceScope contract.EvidenceScope `json:"evidenceScope,omitempty"`
	}{
		RequestID: status.ID, Phase: status.Phase, Stage: status.Stage, Outcome: status.Outcome,
		Reason: status.Reason, TaskName: status.TaskName, TaskUID: status.TaskUID,
		EvidenceScope: status.EvidenceScope,
	}); err != nil {
		return errors.New("could not print remediation status")
	}
	return nil
}

func waitRemediation(ctx context.Context, cmd *cobra.Command, api *remediationClient, status *contract.Status, interval time.Duration) (*contract.Status, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for !contract.TerminalPhase(status.Phase) {
		select {
		case <-ctx.Done():
			return nil, errors.New("waiting stopped; the remote run was not cancelled; use status with the printed requestID")
		case <-ticker.C:
			next, err := api.statusRequest(ctx, http.MethodGet, "/"+status.ID, status.ID, nil)
			if err != nil {
				return nil, err
			}
			if status.TaskUID != "" && (next.TaskName != status.TaskName || next.TaskUID != status.TaskUID) {
				return nil, errors.New("the remediation parent Task identity changed while waiting")
			}
			if next.EvidenceScope != status.EvidenceScope {
				return nil, errors.New("the remediation evidence scope changed while waiting")
			}
			if next.Phase != status.Phase || next.Stage != status.Stage || next.Outcome != status.Outcome ||
				next.Reason != status.Reason || next.TaskName != status.TaskName || next.TaskUID != status.TaskUID {
				if err := printRemediationStatusTo(cmd.ErrOrStderr(), next); err != nil {
					return nil, err
				}
			}
			status = next
		}
	}
	return status, nil
}

func remediationCompletion(status *contract.Status, mode contract.Mode) error {
	if !validRemediationFeedback(status) {
		return errors.New("server returned invalid remediation outcome metadata")
	}
	if status.Phase == contract.PhaseSucceeded {
		if mode == contract.Validate && (status.Outcome == contract.OutcomeReproduced || status.Outcome == contract.OutcomeNotReproduced) {
			return nil
		}
		if mode != contract.Validate && status.Outcome == contract.OutcomeVerified {
			return nil
		}
	}
	message := "remediation did not complete successfully; inspect the remote run; no automatic retry or publication was requested"
	if status.PatchReady {
		message = "the candidate patch is not verified; use download to retain it; no automatic retry or publication was requested"
	}
	if status.Outcome != "" {
		message += "; outcome=" + status.Outcome
	}
	if status.Reason != "" {
		message += "; reason=" + status.Reason
	}
	return errors.New(message)
}
