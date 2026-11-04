package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"k8s.io/apimachinery/pkg/types"
)

const retainedCleanupBodyLimit = 1 << 20

// retainedPoolCleanupTransport binds the actual encoded request, not merely a
// client method name. Old-epoch runtime credentials are pool-wide: they must
// never let a cleanup client target a peer Session or outlive its current owner.
type retainedPoolCleanupTransport struct {
	base         http.RoundTripper
	endpoint     *url.URL
	control      store.DurableControlStore
	owner        store.ControllerEpochFence
	fence        *harnessv2.Fence
	taskUID      types.UID
	taskAttempt  uint32
	promptID     harnessv2.PromptID
	deleteOnly   bool
	finalization runtimeSessionPublicationFinalization
	verify       func(context.Context) error
}

func (t *retainedPoolCleanupTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != t.endpoint.Scheme || request.URL.Host != t.endpoint.Host || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Fragment != "" {
		return nil, errors.New("retained cleanup request changed its exact endpoint")
	}
	if request.Method == http.MethodGet {
		if request.URL.Path != harnessv2.StatusPath && request.URL.Path != harnessv2.CapabilitiesPath {
			return nil, errors.New("retained cleanup cannot read unrelated runtime routes")
		}
		return t.base.RoundTrip(request)
	}
	if request.Body == nil {
		return nil, errors.New("retained cleanup mutation lacks its bound body")
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, retainedCleanupBodyLimit+1))
	closeErr := request.Body.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(body) > retainedCleanupBodyLimit {
		return nil, errors.New("retained cleanup mutation exceeds bounded metadata size")
	}
	if err := t.validateMutation(request, body); err != nil {
		return nil, err
	}
	var response *http.Response
	// Hold the authoritative interlock through transmission and the bounded
	// response body. The old runtime epoch alone cannot fence out a new leader.
	err = agentRuntimeRecoveryGuard(request.Context(), t.control, t.owner, func(guardCtx context.Context) error {
		if err := t.verify(guardCtx); err != nil {
			return err
		}
		bound := request.Clone(guardCtx)
		bound.Body = io.NopCloser(bytes.NewReader(body))
		bound.ContentLength = int64(len(body))
		var sendErr error
		response, sendErr = t.base.RoundTrip(bound)
		if sendErr != nil {
			return sendErr
		}
		result, readErr := io.ReadAll(io.LimitReader(response.Body, retainedCleanupBodyLimit+1))
		bodyCloseErr := response.Body.Close()
		if readErr != nil {
			return readErr
		}
		if bodyCloseErr != nil {
			return bodyCloseErr
		}
		if len(result) > retainedCleanupBodyLimit {
			return errors.New("retained cleanup response exceeds bounded metadata size")
		}
		response.Body = io.NopCloser(bytes.NewReader(result))
		response.ContentLength = int64(len(result))
		return nil
	})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, err
	}
	return response, nil
}

func (t *retainedPoolCleanupTransport) validateMutation(request *http.Request, body []byte) error {
	var envelope struct {
		Protocol string                     `json:"protocol"`
		Metadata harnessv2.MutationMetadata `json:"metadata"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return errors.New("retained cleanup mutation is not valid protocol JSON")
	}
	metadata := envelope.Metadata
	if envelope.Protocol != harnessv2.ProtocolVersion || harnessv2.CompareFence(*t.fence, metadata.Fence, true) != harnessv2.FenceMatch ||
		metadata.TaskUID != harnessv2.TaskUID(t.taskUID) || metadata.TaskAttempt != t.taskAttempt {
		return fmt.Errorf("%w: retained cleanup mutation changed its frozen Task/session fence", store.ErrConflict)
	}
	sessionID := harnessv2.RuntimeSessionID(runtimeSessionID(*t.fence))
	deletePath, err := harnessv2.RuntimeSessionPath(sessionID)
	if err != nil {
		return err
	}
	if request.Method == http.MethodDelete && request.URL.Path == deletePath && metadata.PromptID == "" {
		return nil
	}
	if t.deleteOnly || metadata.PromptID != t.promptID || request.Method != http.MethodPut {
		return errors.New("retained cleanup mutation is outside its exact operation scope")
	}
	cancelPath, err := harnessv2.PromptCancelPath(sessionID, t.promptID)
	if err != nil {
		return err
	}
	deltaID := harnessv2.WorkspaceDeltaID("delta-" + string(t.promptID))
	deltaPath, err := harnessv2.WorkspaceDeltaPath(sessionID, deltaID)
	if err != nil {
		return err
	}
	finalizationPath, err := harnessv2.RuntimeSessionPublicationFinalizationPath(sessionID)
	if err != nil {
		return err
	}
	if request.URL.Path == cancelPath || request.URL.Path == deltaPath {
		return nil
	}
	if request.URL.Path == finalizationPath {
		var finalization harnessv2.FinalizeRuntimeSessionPublicationRequest
		if err := json.Unmarshal(body, &finalization); err != nil {
			return err
		}
		if finalization.WorkspaceDeltaID == deltaID && finalization.TerminalState == t.finalization.TerminalState &&
			finalization.PublicationID == t.finalization.PublicationID && finalization.PublicationGeneration == t.finalization.PublicationGeneration && finalization.PublicationVersion == t.finalization.PublicationVersion && finalization.TerminalReceiptDigest == t.finalization.TerminalReceiptDigest {
			return nil
		}
	}
	return errors.New("retained cleanup cannot retarget validation or claim successful publication")
}
