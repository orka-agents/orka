package controller

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
