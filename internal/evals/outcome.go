package evals

// TrialOutcomeKind is the overall result of one trial.
type TrialOutcomeKind string

const (
	TrialPass TrialOutcomeKind = "pass"
	TrialFail TrialOutcomeKind = "fail"
	// TrialInfraError trials are excluded from scores, not failed.
	TrialInfraError TrialOutcomeKind = "infraError"
)

// InfraClass classifies why a trial produced no gradable outcome. The
// classifier lives with the controller, which sees the Task and Session.
type InfraClass string

const (
	// InfraRuntimeLost: the runtime serving the trial was lost.
	InfraRuntimeLost InfraClass = "RuntimeLost"
	// InfraOutcomeUnknown: the runtime cannot say whether the turn completed.
	InfraOutcomeUnknown InfraClass = "OutcomeUnknown"
	// InfraProviderProxyError: the provider proxy answered with a 5xx.
	InfraProviderProxyError InfraClass = "ProviderProxyError"
	// InfraNoModelOutput: the review Task failed before any model output.
	InfraNoModelOutput InfraClass = "NoModelOutput"
	// InfraTimeoutBeforeInference: the trial timed out before its first
	// inference.
	InfraTimeoutBeforeInference InfraClass = "TimeoutBeforeInference"
)

// ReviewSchemaVersion is the schema of the RepositoryMonitor review result.
const ReviewSchemaVersion = "orka.prReview.v1"

// Review verdicts and test statuses graders inspect.
const (
	ReviewVerdictPassed     = "passed"
	ReviewTestsStatusPassed = "passed"
)

// ReviewResult is the part of a parsed orka.prReview.v1 result that graders
// read. The production parser owns the schema; the review Subject fills this
// view from it, so graders never parse model output themselves.
type ReviewResult struct {
	SchemaVersion string
	HeadSHA       string
	// Verdict is the verdict the reviewer reported, before Orka's gates.
	Verdict     string
	Findings    []ReviewFinding
	TestsStatus string
}

// ReviewFinding is the location of one reported finding.
type ReviewFinding struct {
	File string
	Line int64
}

// ReviewTrialOutcome is what a repositoryMonitorReview trial produced, as the
// production parse-and-gate step saw it.
type ReviewTrialOutcome struct {
	// Result is the parsed review, or nil when none parsed.
	Result *ReviewResult
	// Verdict is the verdict production would record after parsing and
	// gates: a malformed or stale result, or a downgraded pass, changes it.
	Verdict string
	// SkipReason is the production skip reason when the result was rejected.
	SkipReason string
	// GateReason is the verdict gate reason when a pass was downgraded.
	GateReason string
	// ValidationImageConfigured reports whether the monitor ran a validation
	// image that could back a passing tests claim.
	ValidationImageConfigured bool
}

// TrialOutcome is the subject-specific outcome of one trial. Exactly one
// member is set, matching the run's subject type.
type TrialOutcome struct {
	// InfraClass is set when the trial produced no gradable outcome.
	InfraClass InfraClass
	// RepositoryMonitorReview is the review subject's outcome.
	RepositoryMonitorReview *ReviewTrialOutcome
}
