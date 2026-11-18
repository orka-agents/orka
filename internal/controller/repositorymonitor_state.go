package controller

import "github.com/orka-agents/orka/internal/store"

const repositoryMonitorLegacyPlanRequiresReplanning = "legacy_plan_requires_replanning"

func repositoryMonitorNormalizeLegacyIssuePhase(item *store.MonitorItem) {
	if item == nil {
		return
	}
	if item.SkipReason == repositoryMonitorIssueSkipStoppedByCommand {
		switch item.WorkflowPhase {
		case "approved", "approval_required":
			item.WorkflowPhase = repositoryMonitorIssuePhaseBlocked
		}
		return
	}
	switch item.WorkflowPhase {
	case "approved":
		item.WorkflowPhase = repositoryMonitorIssuePhasePlanned
	case "approval_required":
		// This state also covered plans that needed human input. Preserve
		// that hold until a fresh command requests a plan under current policy.
		item.WorkflowPhase = repositoryMonitorIssuePhaseBlocked
		item.SkipReason = repositoryMonitorLegacyPlanRequiresReplanning
	}
}

func repositoryMonitorIssuePhaseTransitionAllowed(from, to string) bool {
	if from == "" || from == to {
		return true
	}
	allowed := map[string]map[string]struct{}{
		repositoryMonitorIssuePhaseDiscovered:           {repositoryMonitorIssuePhaseTriageQueued: {}, repositoryMonitorIssuePhaseResearchQueued: {}, repositoryMonitorIssuePhasePlanQueued: {}, repositoryMonitorIssuePhaseImplementationQueued: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseTriageQueued:         {repositoryMonitorIssuePhaseTriaging: {}, repositoryMonitorIssuePhaseBlocked: {}, repositoryMonitorIssuePhaseDiscovered: {}},
		repositoryMonitorIssuePhaseTriaging:             {repositoryMonitorIssuePhaseTriaged: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseTriaged:              {repositoryMonitorIssuePhaseResearchQueued: {}, repositoryMonitorIssuePhasePlanQueued: {}, repositoryMonitorIssuePhaseImplementationQueued: {}, repositoryMonitorIssuePhaseBlocked: {}, repositoryMonitorIssuePhaseComplete: {}},
		repositoryMonitorIssuePhaseResearchQueued:       {repositoryMonitorIssuePhaseResearching: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseResearching:          {repositoryMonitorIssuePhaseResearched: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseResearched:           {repositoryMonitorIssuePhasePlanQueued: {}, repositoryMonitorIssuePhaseImplementationQueued: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhasePlanQueued:           {repositoryMonitorIssuePhasePlanning: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhasePlanning:             {repositoryMonitorIssuePhasePlanReady: {}, repositoryMonitorIssuePhasePlanned: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhasePlanReady:            {repositoryMonitorIssuePhasePlanned: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhasePlanned:              {repositoryMonitorIssuePhaseImplementationQueued: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseImplementationQueued: {repositoryMonitorIssuePhaseImplementing: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseImplementing:         {repositoryMonitorIssuePhasePatchReady: {}, repositoryMonitorIssuePhaseMutationQueued: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhasePatchReady:           {repositoryMonitorIssuePhaseMutationQueued: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseMutationQueued:       {repositoryMonitorIssuePhaseMutatingToPR: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseMutatingToPR:         {repositoryMonitorIssuePhasePROpened: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseBlocked:              {repositoryMonitorIssuePhaseDiscovered: {}, repositoryMonitorIssuePhaseTriageQueued: {}, repositoryMonitorIssuePhaseResearchQueued: {}, repositoryMonitorIssuePhasePlanQueued: {}, repositoryMonitorIssuePhasePlanned: {}},
	}
	_, ok := allowed[from][to]
	return ok
}
