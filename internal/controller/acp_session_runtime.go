package controller

import (
	"fmt"
	"math"
	"strings"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

const maxControllerRuntimeSessionGeneration = uint64(math.MaxInt64)

// ACPRuntimeSessionBinding is the controller-owned durable identity projected
// into RuntimeSession status. Generation is incremented whenever the provider
// session must be recreated rather than transparently recovered.
type ACPRuntimeSessionBinding struct {
	SessionUID         string
	Generation         uint64
	ProfileDigest      harnessv2.ProfileDigest
	RuntimeInstanceID  harnessv2.RuntimeInstanceID
	SupervisorBootID   harnessv2.SupervisorBootID
	WorkspaceDigest    string
	MCPDigest          string
	RecreationRequired bool
}

// ACPRuntimeSessionPlan explains whether the supervisor session can be reused
// or must be recreated from the verified branch plus bounded transcript.
type ACPRuntimeSessionPlan struct {
	Binding           ACPRuntimeSessionBinding
	Recreate          bool
	BootstrapRequired bool
	Reason            string
}

// PlanACPRuntimeSession selects the next monotonic RuntimeSession generation.
// A profile rotation or proven runtime loss increments exactly once and always
// requires canonical bootstrap; unchanged live state is reused.
func PlanACPRuntimeSession(
	session store.SessionControl,
	current *ACPRuntimeSessionBinding,
	desiredProfileDigest harnessv2.ProfileDigest,
	desiredMCPDigest string,
	desiredRuntimeInstanceID harnessv2.RuntimeInstanceID,
	desiredSupervisorBootID harnessv2.SupervisorBootID,
) (ACPRuntimeSessionPlan, error) {
	if err := store.ValidateControlIdentifier("ACP runtime session UID", strings.TrimSpace(session.SessionUID)); err != nil {
		return ACPRuntimeSessionPlan{}, err
	}
	if session.Availability != store.SessionAvailable {
		return ACPRuntimeSessionPlan{}, fmt.Errorf("%w: ACP runtime session is reconciliation-blocked", store.ErrConflict)
	}
	if err := harnessv2.ValidateProfileDigest(desiredProfileDigest); err != nil {
		return ACPRuntimeSessionPlan{}, fmt.Errorf("desired ACP runtime profile digest: %w", err)
	}
	if err := store.ValidateCanonicalDigest("desired ACP runtime MCP digest", desiredMCPDigest); err != nil {
		return ACPRuntimeSessionPlan{}, err
	}
	if current == nil {
		return ACPRuntimeSessionPlan{
			Binding: ACPRuntimeSessionBinding{
				SessionUID: session.SessionUID, Generation: 1, ProfileDigest: desiredProfileDigest,
				RuntimeInstanceID: desiredRuntimeInstanceID, SupervisorBootID: desiredSupervisorBootID, MCPDigest: desiredMCPDigest,
			},
			Recreate: true, BootstrapRequired: true, Reason: "initial-runtime-session",
		}, nil
	}
	if current.SessionUID != session.SessionUID {
		return ACPRuntimeSessionPlan{}, fmt.Errorf("%w: runtime session UID %q does not match durable Session UID %q",
			store.ErrConflict, current.SessionUID, session.SessionUID)
	}
	if current.Generation == 0 {
		return ACPRuntimeSessionPlan{}, store.ValidationErrorf("current ACP runtime session generation must be at least 1")
	}
	if current.Generation > maxControllerRuntimeSessionGeneration {
		return ACPRuntimeSessionPlan{}, store.ValidationErrorf("current ACP runtime session generation exceeds durable Task status capacity")
	}
	if err := harnessv2.ValidateProfileDigest(current.ProfileDigest); err != nil {
		return ACPRuntimeSessionPlan{}, fmt.Errorf("current ACP runtime profile digest: %w", err)
	}
	if current.MCPDigest != "" {
		if err := store.ValidateCanonicalDigest("current ACP runtime MCP digest", current.MCPDigest); err != nil {
			return ACPRuntimeSessionPlan{}, err
		}
	}
	if current.RecreationRequired {
		sameSupervisor := current.RuntimeInstanceID == desiredRuntimeInstanceID && current.SupervisorBootID == desiredSupervisorBootID
		configurationChanged := current.ProfileDigest != desiredProfileDigest ||
			(current.MCPDigest != "" && current.MCPDigest != desiredMCPDigest)
		pending := *current
		if !sameSupervisor || configurationChanged || current.MCPDigest == "" {
			if pending.Generation >= maxControllerRuntimeSessionGeneration {
				return ACPRuntimeSessionPlan{}, store.ValidationErrorf("ACP runtime session generation is exhausted")
			}
			pending.Generation++
		}
		pending.ProfileDigest = desiredProfileDigest
		pending.MCPDigest = desiredMCPDigest
		pending.RuntimeInstanceID = desiredRuntimeInstanceID
		pending.SupervisorBootID = desiredSupervisorBootID
		return ACPRuntimeSessionPlan{
			Binding: pending, Recreate: true, BootstrapRequired: true, Reason: "runtime-session-recreation-pending",
		}, nil
	}
	profileChanged := current.ProfileDigest != desiredProfileDigest
	mcpChanged := current.MCPDigest == "" || current.MCPDigest != desiredMCPDigest
	runtimeLost := current.RuntimeInstanceID != desiredRuntimeInstanceID || current.SupervisorBootID != desiredSupervisorBootID
	if !profileChanged && !mcpChanged && !runtimeLost {
		return ACPRuntimeSessionPlan{Binding: *current, Reason: "reuse-live-runtime-session"}, nil
	}
	if current.Generation == maxControllerRuntimeSessionGeneration {
		return ACPRuntimeSessionPlan{}, store.ValidationErrorf("ACP runtime session generation is exhausted")
	}
	reason := "runtime-lost"
	if profileChanged {
		reason = "runtime-profile-rotated"
	} else if mcpChanged {
		reason = "runtime-mcp-rotated"
	}
	return ACPRuntimeSessionPlan{
		Binding: ACPRuntimeSessionBinding{
			SessionUID: session.SessionUID, Generation: current.Generation + 1, ProfileDigest: desiredProfileDigest,
			RuntimeInstanceID: desiredRuntimeInstanceID, SupervisorBootID: desiredSupervisorBootID, MCPDigest: desiredMCPDigest,
		},
		Recreate: true, BootstrapRequired: true, Reason: reason,
	}, nil
}

// enforceACPRuntimeSessionGenerationFloor prevents a recreated provider
// RuntimeSession from reusing a generation already committed on durable
// workspace data. A reusable live binding at the exact floor remains valid.
func enforceACPRuntimeSessionGenerationFloor(
	plan ACPRuntimeSessionPlan,
	floor uint64,
) (ACPRuntimeSessionPlan, error) {
	needsAdvance := plan.Binding.Generation < floor ||
		(plan.Recreate && plan.Binding.Generation <= floor)
	if !needsAdvance {
		return plan, nil
	}
	if floor >= maxControllerRuntimeSessionGeneration {
		return ACPRuntimeSessionPlan{}, store.ValidationErrorf("ACP runtime session generation is exhausted")
	}
	plan.Binding.Generation = floor + 1
	plan.Recreate = true
	plan.BootstrapRequired = true
	plan.Reason = "durable-workspace-generation-floor"
	return plan, nil
}

// bindACPRuntimeSessionWorkspace binds the exact prepared workspace identity to
// a provider RuntimeSession. A live session may be reused only when repository,
// source ref, verified baseline, intent, and relative root all match. Initial or
// already-rotated sessions simply record the desired digest; a mismatch on a
// reused live session advances the monotonic generation exactly once.
func bindACPRuntimeSessionWorkspace(
	binding ACPRuntimeSessionBinding,
	reused bool,
	desiredWorkspaceDigest string,
) (ACPRuntimeSessionBinding, bool, error) {
	desiredWorkspaceDigest = strings.TrimSpace(desiredWorkspaceDigest)
	if err := store.ValidateCanonicalDigest("desired ACP runtime workspace digest", desiredWorkspaceDigest); err != nil {
		return ACPRuntimeSessionBinding{}, false, err
	}
	if binding.WorkspaceDigest != "" {
		if err := store.ValidateCanonicalDigest("current ACP runtime workspace digest", binding.WorkspaceDigest); err != nil {
			return ACPRuntimeSessionBinding{}, false, err
		}
	}
	if binding.WorkspaceDigest == desiredWorkspaceDigest {
		return binding, false, nil
	}
	if binding.RecreationRequired && binding.WorkspaceDigest != "" {
		if binding.Generation >= maxControllerRuntimeSessionGeneration {
			return ACPRuntimeSessionBinding{}, false, store.ValidationErrorf("ACP runtime session generation is exhausted")
		}
		binding.Generation++
		binding.WorkspaceDigest = desiredWorkspaceDigest
		return binding, false, nil
	}
	if reused {
		if binding.Generation >= maxControllerRuntimeSessionGeneration {
			return ACPRuntimeSessionBinding{}, false, store.ValidationErrorf("ACP runtime session generation is exhausted")
		}
		binding.Generation++
	}
	binding.WorkspaceDigest = desiredWorkspaceDigest
	return binding, reused, nil
}
