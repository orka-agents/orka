// Package usage builds cohort reports from retained counts and verified links.
package usage

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

const ReadinessMaxAge = 10 * time.Minute

const (
	unknownTaskPhase = "Unknown"
	unassociatedRole = "unassociated"
	usageUnavailable = "unavailable"
	usagePartial     = "partial"
	usageComplete    = "complete"
)

type Totals struct {
	InputTokens             int64  `json:"inputTokens"`
	OutputTokens            int64  `json:"outputTokens"`
	CachedInputTokens       int64  `json:"cachedInputTokens"`
	CacheWriteInputTokens   int64  `json:"cacheWriteInputTokens"`
	TotalTokens             int64  `json:"totalTokens"`
	EstimatedTokens         int64  `json:"estimatedTokens"`
	CachedUsageReported     bool   `json:"cachedUsageReported"`
	CacheWriteUsageReported bool   `json:"cacheWriteUsageReported"`
	Measurements            int    `json:"measurements"`
	ReportedMeasurements    int    `json:"reportedMeasurements"`
	MissingMeasurements     int    `json:"missingMeasurements"`
	PartialMeasurements     int    `json:"partialMeasurements"`
	Calls                   int    `json:"calls"`
	Attempts                int    `json:"attempts"`
	Completeness            string `json:"completeness"`
	ModelCost               string `json:"modelCost"`
}

type Summary struct {
	Totals
	WorkRequests      int      `json:"workRequests"`
	UnfinishedWork    int      `json:"unfinishedWork"`
	PRsOpened         int      `json:"prsOpened"`
	PRsReady          int      `json:"prsReady"`
	PRsMerged         int      `json:"prsMerged"`
	PRsClosed         int      `json:"prsClosed"`
	PRsAssisted       int      `json:"prsAssisted"`
	TokensPerPROpened *float64 `json:"tokensPerPROpened"`
	TokensPerPRMerged *float64 `json:"tokensPerPRMerged"`
}

type Measurement struct {
	ID                    string    `json:"id"`
	AttemptID             string    `json:"attemptID,omitempty"`
	Scope                 string    `json:"scope"`
	Source                string    `json:"source"`
	Provider              string    `json:"provider,omitempty"`
	Model                 string    `json:"model,omitempty"`
	InputTokens           *int64    `json:"inputTokens"`
	OutputTokens          *int64    `json:"outputTokens"`
	CachedInputTokens     *int64    `json:"cachedInputTokens"`
	CacheWriteInputTokens *int64    `json:"cacheWriteInputTokens"`
	Status                string    `json:"status"`
	Completeness          string    `json:"completeness"`
	Gap                   string    `json:"gap,omitempty"`
	ObservedAt            time.Time `json:"observedAt"`
	complete              bool
}

type Task struct {
	store.UsageTask
	Totals       Totals        `json:"usage"`
	Measurements []Measurement `json:"measurements"`
	Shared       bool          `json:"shared"`
}

type PullRequest struct {
	store.UsagePullRequest
	Origin     string `json:"origin"`
	EvidenceID string `json:"evidenceID"`
}

type Work struct {
	store.UsageWorkRequest
	Summary      Summary       `json:"summary"`
	Tasks        []Task        `json:"tasks"`
	PullRequests []PullRequest `json:"pullRequests"`
	Models       []string      `json:"models"`
}

type Team struct {
	Namespace string  `json:"namespace"`
	Summary   Summary `json:"summary"`
}

type OtherWork struct {
	Category    string `json:"category"`
	Explanation string `json:"explanation"`
	Totals      Totals `json:"usage"`
	Tasks       []Task `json:"tasks"`
}

type Report struct {
	Selection     store.UsageFilter `json:"selection"`
	RetainedSince *time.Time        `json:"retainedSince,omitempty"`
	Summary       Summary           `json:"summary"`
	Teams         []Team            `json:"teams"`
	Works         []Work            `json:"works"`
	OtherWork     []OtherWork       `json:"otherWork"`
}

type counter struct {
	counts [4]*int64
	byTask map[measurementOwner]*Measurement
	gap    string
}

type measurementOwner struct{ taskID, attemptID string }

func taskKey(namespace, uid string) string { return namespace + "/" + uid }
func prKey(repository string, number int64) string {
	return fmt.Sprintf("%s#%d", strings.ToLower(repository), number)
}

// Build selects requests by start date. Model filtering selects whole requests
// that used a model; it never removes their other models from the numerator.
func Build(data store.UsageData, filter store.UsageFilter) Report {
	report := Report{Selection: filter, RetainedSince: data.RetainedSince, Teams: []Team{}, Works: []Work{}, OtherWork: []OtherWork{}}
	tasks := measuredTasks(data, filter.AsOf)
	prs := pullRequestsAsOf(data.PullRequests, filter.AsOf)
	links := map[string][]store.UsagePRLink{}
	for _, link := range data.Links {
		if !link.LinkedAt.After(filter.AsOf) {
			links[link.WorkID] = append(links[link.WorkID], link)
		}
	}
	// PR-targeted review/repair belongs to the original request when a trusted
	// publication links it. Multiple requests can share the same Task.
	workTasks := map[string]map[string]Task{}
	for _, work := range data.Works {
		members := map[string]Task{}
		for key, task := range tasks {
			if task.Namespace != work.Namespace {
				continue
			}
			if task.WorkID == work.ID {
				members[key] = task
				continue
			}
			for _, link := range links[work.ID] {
				if task.PRNumber > 0 && task.PRNumber == link.Number && task.Repository == link.Repository {
					members[key] = task
				}
			}
		}
		workTasks[work.ID] = members
	}
	selectedTasks := map[string]Task{}
	teamTasks := map[string]map[string]Task{}
	teamWorks := map[string][]Work{}
	for _, request := range data.Works {
		if request.Kind != "issue" || !inPeriod(request.StartedAt, filter) ||
			(filter.Repository != "" && !strings.EqualFold(request.Repository, filter.Repository)) ||
			(filter.Kind != "" && filter.Kind != "issue") {
			continue
		}
		members := workTasks[request.ID]
		if filter.Model != "" && !tasksUseModel(members, filter.Model) {
			continue
		}
		work := Work{UsageWorkRequest: request, Tasks: []Task{}, PullRequests: []PullRequest{}, Models: []string{}}
		for _, link := range links[request.ID] {
			pr, ok := prs[taskKey(request.Namespace, prKey(link.Repository, link.Number))]
			if !ok {
				pr = store.UsagePullRequest{Namespace: request.Namespace, Repository: link.Repository, Number: link.Number,
					URL: fmt.Sprintf("https://github.com/%s/pull/%d", link.Repository, link.Number), State: "unknown", ReadinessReason: "GitHub state has not been observed"}
			}
			work.PullRequests = append(work.PullRequests, PullRequest{UsagePullRequest: pr, Origin: link.Origin, EvidenceID: link.EvidenceID})
		}
		for key, task := range members {
			if task.WorkID != request.ID {
				task.Shared = true
			}
			work.Tasks = append(work.Tasks, task)
			selectedTasks[key] = task
			if teamTasks[request.Namespace] == nil {
				teamTasks[request.Namespace] = map[string]Task{}
			}
			teamTasks[request.Namespace][key] = task
			for _, measurement := range task.Measurements {
				if measurement.Model != "" && !slices.Contains(work.Models, measurement.Model) {
					work.Models = append(work.Models, measurement.Model)
				}
			}
		}
		sort.Slice(work.Tasks, func(i, j int) bool { return work.Tasks[i].StartedAt.Before(work.Tasks[j].StartedAt) })
		sort.Slice(work.PullRequests, func(i, j int) bool { return work.PullRequests[i].Number < work.PullRequests[j].Number })
		slices.Sort(work.Models)
		work.Summary = summarize([]Work{work}, members)
		report.Works = append(report.Works, work)
		teamWorks[request.Namespace] = append(teamWorks[request.Namespace], work)
	}
	sort.Slice(report.Works, func(i, j int) bool {
		if report.Works[i].StartedAt.Equal(report.Works[j].StartedAt) {
			return report.Works[i].ID < report.Works[j].ID
		}
		return report.Works[i].StartedAt.Before(report.Works[j].StartedAt)
	})
	report.Summary = summarize(report.Works, selectedTasks)
	for _, namespace := range filter.Namespaces {
		report.Teams = append(report.Teams, Team{Namespace: namespace, Summary: summarize(teamWorks[namespace], teamTasks[namespace])})
	}
	report.OtherWork = otherWork(tasks, selectedTasks, data.Works, filter)
	return report
}

func measuredTasks(data store.UsageData, asOf time.Time) map[string]Task {
	tasks := map[string]Task{}
	for _, task := range data.Tasks {
		if len(task.PhaseHistory) > 0 {
			task.Phase = unknownTaskPhase
			for _, state := range task.PhaseHistory {
				if !state.ObservedAt.After(asOf) {
					task.Phase = state.Phase
				}
			}
		}
		task.PhaseHistory = nil
		if !task.StartedAt.After(asOf) {
			tasks[taskKey(task.Namespace, task.TaskUID)] = Task{UsageTask: task, Measurements: []Measurement{}}
		}
	}
	observations := slices.Clone(data.Observations)
	sort.SliceStable(observations, func(i, j int) bool { return observations[i].ObservedAt.Before(observations[j].ObservedAt) })
	counters := map[string]*counter{}
	seen := map[string]bool{}
	for _, observation := range observations {
		id := taskKey(observation.Namespace, observation.ID)
		if seen[id] || observation.ObservedAt.After(asOf) {
			continue
		}
		seen[id] = true
		key := taskKey(observation.Namespace, observation.Scope+"/"+observation.CounterID)
		current := counters[key]
		if current == nil {
			current = &counter{byTask: map[measurementOwner]*Measurement{}}
			counters[key] = current
		}
		uid := observation.TaskUID
		if uid == "" {
			uid = "call-" + observation.CounterID
		}
		tk := taskKey(observation.Namespace, uid)
		if _, exists := tasks[tk]; !exists {
			tasks[tk] = Task{UsageTask: store.UsageTask{Namespace: observation.Namespace, TaskUID: uid, TaskName: observation.TaskName,
				SessionName: observation.SessionName, StartedAt: observation.ObservedAt, Role: unassociatedRole, Phase: unknownTaskPhase}, Measurements: []Measurement{}}
		}
		if baselineKey := addSessionBaseline(current, observation, tasks); baselineKey != "" && data.HiddenTasks[tk] {
			data.HiddenTasks[baselineKey] = true
		}
		applyObservation(current, observation, tk)
	}
	for key, c := range counters {
		for owner, measurement := range c.byTask {
			measurement.ID = key + "/" + owner.attemptID
			measurement.Gap = c.gap
			measurement.Completeness = usageUnavailable
			if measurement.InputTokens != nil || measurement.OutputTokens != nil {
				measurement.Completeness = usagePartial
				if measurement.complete && c.gap == "" && measurement.InputTokens != nil && measurement.OutputTokens != nil {
					measurement.Completeness = usageComplete
				}
			}
			if measurement.Completeness == usageUnavailable && measurement.Gap == "" {
				measurement.Gap = "No consumed-token counts reported"
			}
			task := tasks[owner.taskID]
			task.Measurements = append(task.Measurements, *measurement)
			tasks[owner.taskID] = task
		}
	}
	for key, task := range tasks {
		if data.HiddenTasks[key] {
			delete(tasks, key)
			continue
		}
		tasks[key] = finishTaskMeasurements(task, key)
	}
	return tasks
}

// The first session total may predate this Task. Keep it visible as
// unassociated usage and attribute only subsequent deltas to Tasks.
func addSessionBaseline(current *counter, observation store.UsageObservation, tasks map[string]Task) string {
	if observation.Scope != store.UsageScopeSession {
		return ""
	}
	baselineUID := "baseline-" + observation.CounterID
	baselineKey := taskKey(observation.Namespace, baselineUID)
	baselineOwner := measurementOwner{taskID: baselineKey}
	counts := [4]*int64{observation.InputTokens, observation.OutputTokens, observation.CachedInputTokens, observation.CacheWriteInputTokens}
	added := false
	for i, count := range counts {
		if count == nil || current.counts[i] != nil || *count == 0 {
			continue
		}
		baseline := current.byTask[baselineOwner]
		if baseline == nil {
			baseline = &Measurement{Scope: store.UsageScopeSession, Source: observation.Source, Provider: observation.Provider,
				Model: observation.Model, Status: "unattributed", ObservedAt: observation.ObservedAt}
			current.byTask[baselineOwner] = baseline
			tasks[baselineKey] = Task{UsageTask: store.UsageTask{Namespace: observation.Namespace, TaskUID: baselineUID,
				SessionName: observation.SessionName, Role: unassociatedRole, Phase: unknownTaskPhase, StartedAt: observation.ObservedAt}, Measurements: []Measurement{}}
		}
		targets := [4]**int64{&baseline.InputTokens, &baseline.OutputTokens, &baseline.CachedInputTokens, &baseline.CacheWriteInputTokens}
		v := *count
		*targets[i] = &v
		added = true
	}
	if added {
		return baselineKey
	}
	return ""
}

func finishTaskMeasurements(task Task, key string) Task {
	// ACP lifecycle events describe the prompt attempt. A session counter
	// for the same prompt replaces its empty lifecycle measurement.
	sessionAttempts := map[string]bool{}
	for _, m := range task.Measurements {
		if m.Scope == store.UsageScopeSession && m.AttemptID != "" {
			sessionAttempts[m.AttemptID] = true
		}
	}
	for _, lifecycle := range task.Measurements {
		if lifecycle.Scope != store.UsageScopeAttempt || lifecycle.InputTokens != nil || lifecycle.OutputTokens != nil {
			continue
		}
		for i := range task.Measurements {
			m := &task.Measurements[i]
			if m.Scope == store.UsageScopeSession && m.AttemptID != "" && m.AttemptID == lifecycle.AttemptID {
				if terminalUsageStatus(lifecycle.Status) {
					m.Status = lifecycle.Status
				}
			}
		}
	}
	task.Measurements = slices.DeleteFunc(task.Measurements, func(m Measurement) bool {
		return m.Scope == store.UsageScopeAttempt && m.InputTokens == nil && m.OutputTokens == nil && sessionAttempts[m.AttemptID]
	})
	if len(task.Measurements) == 0 && task.Runtime != "container" {
		task.Measurements = append(task.Measurements, Measurement{ID: key + "/unreported", Scope: store.UsageScopeAttempt, Source: store.UsageSourceAgent,
			Status: task.Phase, Completeness: usageUnavailable, Gap: "Attempt-level reporting only; no consumed-token counts reported", ObservedAt: task.StartedAt})
	}
	sort.Slice(task.Measurements, func(i, j int) bool {
		if task.Measurements[i].ObservedAt.Equal(task.Measurements[j].ObservedAt) {
			return task.Measurements[i].ID < task.Measurements[j].ID
		}
		return task.Measurements[i].ObservedAt.Before(task.Measurements[j].ObservedAt)
	})
	task.Totals = totalMeasurements(task.Measurements)
	return task
}

func applyObservation(c *counter, observation store.UsageObservation, taskID string) {
	owner := measurementOwner{taskID: taskID, attemptID: observation.AttemptID}
	m := c.byTask[owner]
	if m == nil {
		m = &Measurement{AttemptID: observation.AttemptID, Scope: observation.Scope, Source: observation.Source}
		c.byTask[owner] = m
	}
	m.ObservedAt = observation.ObservedAt
	if observation.Provider != "" {
		m.Provider = observation.Provider
	}
	if observation.Model != "" {
		m.Model = observation.Model
	}
	if m.Status != store.UsageStatusCompleted && m.Status != store.UsageStatusFailed && m.Status != store.UsageStatusCancelled {
		m.Status = observation.Status
	}
	counts := [4]*int64{observation.InputTokens, observation.OutputTokens, observation.CachedInputTokens, observation.CacheWriteInputTokens}
	targets := [4]**int64{&m.InputTokens, &m.OutputTokens, &m.CachedInputTokens, &m.CacheWriteInputTokens}
	for i, count := range counts {
		if count == nil {
			continue
		}
		previous := int64(0)
		if c.counts[i] != nil {
			previous = *c.counts[i]
		}
		if *count < previous {
			c.gap = "Cumulative counter decreased; retained the previously reported usage"
			continue
		}
		if observation.Scope == store.UsageScopeSession && c.counts[i] == nil {
			if observation.Status != store.UsageStatusStarted && *count > 0 {
				c.gap = "Session counter has no initial baseline; earlier usage is shown separately without a work reference"
				v := *count
				c.counts[i] = &v
				continue
			}
			previous = *count
		}
		if *targets[i] == nil {
			v := int64(0)
			*targets[i] = &v
		}
		**targets[i] += *count - previous
		v := *count
		c.counts[i] = &v
	}
	if observation.Complete {
		m.complete = true
	}
}

func terminalUsageStatus(status string) bool {
	return status == store.UsageStatusCompleted || status == store.UsageStatusFailed || status == store.UsageStatusCancelled
}

func totalMeasurements(measurements []Measurement) Totals {
	total := Totals{ModelCost: "Price unavailable"}
	for _, m := range measurements {
		total.Measurements++
		if m.Scope == store.UsageScopeCall {
			total.Calls++
		} else {
			total.Attempts++
		}
		switch m.Completeness {
		case usageComplete:
			total.ReportedMeasurements++
		case usagePartial:
			total.ReportedMeasurements++
			total.PartialMeasurements++
		default:
			total.MissingMeasurements++
		}
		if m.InputTokens != nil {
			total.InputTokens += *m.InputTokens
		}
		if m.OutputTokens != nil {
			total.OutputTokens += *m.OutputTokens
		}
		if m.CachedInputTokens != nil {
			total.CachedUsageReported = true
			total.CachedInputTokens += *m.CachedInputTokens
		}
		if m.CacheWriteInputTokens != nil {
			total.CacheWriteUsageReported = true
			total.CacheWriteInputTokens += *m.CacheWriteInputTokens
		}
		if m.Source == store.UsageSourceEstimate {
			if m.InputTokens != nil {
				total.EstimatedTokens += *m.InputTokens
			}
			if m.OutputTokens != nil {
				total.EstimatedTokens += *m.OutputTokens
			}
		}
	}
	total.TotalTokens = total.InputTokens + total.OutputTokens
	total.Completeness = usageComplete
	if total.ReportedMeasurements == 0 {
		total.Completeness = usageUnavailable
	} else if total.MissingMeasurements > 0 || total.PartialMeasurements > 0 {
		total.Completeness = usagePartial
	}
	return total
}

func summarize(works []Work, tasks map[string]Task) Summary {
	measurements := []Measurement{}
	for _, task := range tasks {
		measurements = append(measurements, task.Measurements...)
	}
	summary := Summary{Totals: totalMeasurements(measurements), WorkRequests: len(works)}
	prs := map[string]PullRequest{}
	for _, work := range works {
		unfinished := len(work.Tasks) == 0
		for _, task := range work.Tasks {
			if task.Phase != "Succeeded" && task.Phase != "Failed" && task.Phase != "Cancelled" {
				unfinished = true
			}
		}
		for _, pr := range work.PullRequests {
			if pr.State == "open" || pr.State == "unknown" {
				unfinished = true
			}
			key := prKey(pr.Repository, pr.Number)
			previous, ok := prs[key]
			if !ok {
				prs[key] = pr
				continue
			}
			origin := pr.Origin
			if previous.Origin == store.UsagePRCreated {
				origin = previous.Origin
			}
			mergedAt := pr.MergedAt
			if previous.MergedAt != nil {
				mergedAt = previous.MergedAt
			}
			if previous.ObservedAt.After(pr.ObservedAt) {
				pr = previous
			}
			pr.Origin = origin
			pr.MergedAt = mergedAt
			if mergedAt != nil {
				pr.State, pr.Ready = "merged", false
			}
			prs[key] = pr
		}
		if unfinished {
			summary.UnfinishedWork++
		}
	}
	for _, pr := range prs {
		if pr.Origin == store.UsagePRAssisted {
			summary.PRsAssisted++
			continue
		}
		if pr.Origin != store.UsagePRCreated {
			continue
		}
		summary.PRsOpened++
		if pr.Ready {
			summary.PRsReady++
		}
		if pr.MergedAt != nil {
			summary.PRsMerged++
		} else if pr.State == "closed" {
			summary.PRsClosed++
		}
	}
	if summary.PRsOpened > 0 && summary.ReportedMeasurements > 0 {
		v := float64(summary.TotalTokens) / float64(summary.PRsOpened)
		summary.TokensPerPROpened = &v
	}
	if summary.PRsMerged > 0 && summary.ReportedMeasurements > 0 {
		v := float64(summary.TotalTokens) / float64(summary.PRsMerged)
		summary.TokensPerPRMerged = &v
	}
	return summary
}

func pullRequestsAsOf(observations []store.UsagePullRequest, asOf time.Time) map[string]store.UsagePullRequest {
	result := map[string]store.UsagePullRequest{}
	for _, pr := range observations {
		if pr.ObservedAt.After(asOf) {
			continue
		}
		key := taskKey(pr.Namespace, prKey(pr.Repository, pr.Number))
		if previous, ok := result[key]; ok {
			mergedAt := pr.MergedAt
			if previous.MergedAt != nil {
				mergedAt = previous.MergedAt
			}
			if pr.ObservedAt.Before(previous.ObservedAt) {
				pr = previous
			}
			pr.MergedAt = mergedAt
		}
		if pr.MergedAt != nil {
			if pr.MergedAt.After(asOf) {
				pr.MergedAt = nil
			} else {
				pr.State, pr.Ready = "merged", false
			}
		}
		if pr.Ready && asOf.Sub(pr.ObservedAt) > ReadinessMaxAge {
			pr.Ready = false
			pr.ReadinessReason = "Readiness observation is stale"
		}
		result[key] = pr
	}
	return result
}

func inPeriod(at time.Time, filter store.UsageFilter) bool {
	return !at.Before(filter.From) && at.Before(filter.Until) && !at.After(filter.AsOf)
}

func tasksUseModel(tasks map[string]Task, model string) bool {
	for _, task := range tasks {
		for _, m := range task.Measurements {
			if m.Model == model {
				return true
			}
		}
	}
	return false
}

func otherWork(tasks, selected map[string]Task, works []store.UsageWorkRequest, filter store.UsageFilter) []OtherWork {
	workKinds := map[string]string{}
	for _, work := range works {
		workKinds[work.ID] = work.Kind
	}
	categories := []OtherWork{
		{Category: "review_only", Explanation: "Review or repair of existing PRs; excluded from PR delivery counts", Tasks: []Task{}},
		{Category: "other_requests", Explanation: "Work outside the selected request cohort", Tasks: []Task{}},
		{Category: unassociatedRole, Explanation: "Chat or usage without a verified work reference; excluded from PR delivery counts", Tasks: []Task{}},
	}
	for key, task := range tasks {
		if len(task.Measurements) == 0 {
			continue
		}
		if _, ok := selected[key]; ok || !inPeriod(task.StartedAt, filter) {
			continue
		}
		if filter.Repository != "" && !strings.EqualFold(task.Repository, filter.Repository) {
			continue
		}
		if filter.Model != "" && !tasksUseModel(map[string]Task{key: task}, filter.Model) {
			continue
		}
		kind := workKinds[task.WorkID]
		if kind == "" && task.PRNumber > 0 {
			kind = "pull_request"
		}
		if filter.Kind != "" && kind != filter.Kind {
			continue
		}
		index := 2
		if workKinds[task.WorkID] == "pull_request" || task.PRNumber > 0 {
			index = 0
		} else if task.WorkID != "" {
			index = 1
		}
		categories[index].Tasks = append(categories[index].Tasks, task)
	}
	for i := range categories {
		measurements := []Measurement{}
		sort.Slice(categories[i].Tasks, func(a, b int) bool { return categories[i].Tasks[a].TaskUID < categories[i].Tasks[b].TaskUID })
		for _, task := range categories[i].Tasks {
			measurements = append(measurements, task.Measurements...)
		}
		categories[i].Totals = totalMeasurements(measurements)
	}
	return categories
}
