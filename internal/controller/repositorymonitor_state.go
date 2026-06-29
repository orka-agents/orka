package controller

func repositoryMonitorIssuePhaseTransitionAllowed(from, to string) bool {
	if from == "" || from == to {
		return true
	}
	allowed := map[string]map[string]struct{}{
		repositoryMonitorIssuePhaseDiscovered:           {repositoryMonitorIssuePhaseTriageQueued: {}, repositoryMonitorIssuePhaseResearchQueued: {}, repositoryMonitorIssuePhasePlanQueued: {}, repositoryMonitorIssuePhaseImplementationQueued: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseTriageQueued:         {repositoryMonitorIssuePhaseTriaging: {}, repositoryMonitorIssuePhaseBlocked: {}, repositoryMonitorIssuePhaseDiscovered: {}},
		repositoryMonitorIssuePhaseTriaging:             {repositoryMonitorIssuePhaseTriaged: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseTriaged:              {repositoryMonitorIssuePhaseResearchQueued: {}, repositoryMonitorIssuePhasePlanQueued: {}, repositoryMonitorIssuePhaseBlocked: {}, repositoryMonitorIssuePhaseComplete: {}},
		repositoryMonitorIssuePhaseResearchQueued:       {repositoryMonitorIssuePhaseResearching: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseResearching:          {repositoryMonitorIssuePhaseResearched: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseResearched:           {repositoryMonitorIssuePhasePlanQueued: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhasePlanQueued:           {repositoryMonitorIssuePhasePlanning: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhasePlanning:             {repositoryMonitorIssuePhasePlanReady: {}, repositoryMonitorIssuePhaseApprovalRequired: {}, repositoryMonitorIssuePhaseApproved: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhasePlanReady:            {repositoryMonitorIssuePhaseApprovalRequired: {}, repositoryMonitorIssuePhaseApproved: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseApprovalRequired:     {repositoryMonitorIssuePhaseApproved: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseApproved:             {repositoryMonitorIssuePhaseImplementationQueued: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseImplementationQueued: {repositoryMonitorIssuePhaseImplementing: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseImplementing:         {repositoryMonitorIssuePhasePatchReady: {}, repositoryMonitorIssuePhaseMutationQueued: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhasePatchReady:           {repositoryMonitorIssuePhaseMutationQueued: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseMutationQueued:       {repositoryMonitorIssuePhaseMutatingToPR: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseMutatingToPR:         {repositoryMonitorIssuePhasePROpened: {}, repositoryMonitorIssuePhaseBlocked: {}},
		repositoryMonitorIssuePhaseBlocked:              {repositoryMonitorIssuePhaseDiscovered: {}, repositoryMonitorIssuePhaseTriageQueued: {}, repositoryMonitorIssuePhaseResearchQueued: {}, repositoryMonitorIssuePhasePlanQueued: {}, repositoryMonitorIssuePhaseApproved: {}},
	}
	_, ok := allowed[from][to]
	return ok
}

func repositoryMonitorPRPhaseTransitionAllowed(from, to string) bool {
	if from == "" || from == to {
		return true
	}
	allowed := map[string]map[string]struct{}{
		"discovered":             {"review_queued": {}, "blocked": {}, "closed": {}},
		"review_queued":          {"reviewing": {}, "blocked": {}, "closed": {}},
		"reviewing":              {"reviewed_passed": {}, "reviewed_needs_changes": {}, "blocked": {}, "closed": {}},
		"reviewed_passed":        {"merge_ready": {}, "head_updated": {}, "ci_failed": {}, "blocked": {}, "closed": {}},
		"reviewed_needs_changes": {"repair_queued": {}, "head_updated": {}, "blocked": {}, "closed": {}},
		"ci_failed":              {"repair_queued": {}, "head_updated": {}, "blocked": {}, "closed": {}},
		"repair_queued":          {"repairing": {}, "blocked": {}, "closed": {}},
		"repairing":              {"head_updated": {}, "review_queued": {}, "blocked": {}, "closed": {}},
		"head_updated":           {"review_queued": {}, "blocked": {}, "closed": {}},
		"merge_ready":            {"head_updated": {}, "blocked": {}, "closed": {}},
		"blocked":                {"review_queued": {}, "repair_queued": {}, "closed": {}},
	}
	_, ok := allowed[from][to]
	return ok
}
