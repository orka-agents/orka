/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package capabilitygap records bounded, content-free signals that a Task
// asked Orka for something it could not do: an option or runtime the
// controller refused, or a tool that did not exist or was not allowed.
//
// Every label is a closed enum or a name from a tool catalog fixed at startup.
// Task references are Kubernetes object names. Prompts, completions, tool
// arguments, error messages and other user text are never accepted, so they
// cannot reach a metric label or the summary.
//
// Recording is off by default. Until a Recorder is installed with SetDefault,
// Record does nothing.
package capabilitygap

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/orka-agents/orka/internal/metrics"
)

// Category groups signals by the kind of gap they report.
type Category string

const (
	// CategoryUnsupportedRequest: the controller refused a Task because it asked
	// for an option or runtime Orka does not support in that configuration.
	CategoryUnsupportedRequest Category = "unsupported_request"
	// CategoryMissingTool: an agent called a tool that does not exist, is not
	// enabled for its Task, or is denied by the Task's frozen tool policy.
	CategoryMissingTool Category = "missing_tool"
)

// Reason narrows a Category to one closed value.
type Reason string

// Reasons for CategoryUnsupportedRequest.
const (
	ReasonRuntime                Reason = "runtime"
	ReasonContractVersion        Reason = "contract_version"
	ReasonExecutionWorkspace     Reason = "execution_workspace"
	ReasonToolboxes              Reason = "toolboxes"
	ReasonPriorTaskRef           Reason = "prior_task_ref"
	ReasonMaxTurns               Reason = "max_turns"
	ReasonRetryPolicy            Reason = "retry_policy"
	ReasonResources              Reason = "resources"
	ReasonPlacement              Reason = "placement"
	ReasonAutonomousCoordination Reason = "autonomous_coordination"
	ReasonTransactionDelegation  Reason = "transaction_delegation"
	ReasonNativeSession          Reason = "native_session"
)

// Reasons for CategoryMissingTool.
const (
	ReasonToolNotRegistered  Reason = "not_registered"
	ReasonToolNotEnabled     Reason = "not_enabled"
	ReasonToolDeniedByPolicy Reason = "denied_by_policy"
)

// ReasonOther replaces a reason that does not belong to its category.
const ReasonOther Reason = "other"

// Runtime label values.
const (
	RuntimeNative   = "native"
	RuntimeCodex    = "codex"
	RuntimeClaude   = "claude"
	RuntimeCopilot  = "copilot"
	RuntimeOpencode = "opencode"
	RuntimeExternal = "external"
	RuntimeUnknown  = "unknown"
)

const (
	// ToolUnknown is the label for a tool name outside the catalog.
	ToolUnknown = "unknown"
	// ToolNone is the label for categories that do not name a tool.
	ToolNone = "none"
)

const (
	// WindowDays is how many UTC days the summary covers, today included.
	WindowDays = 7
	// MaxTaskRefsPerSignal bounds the Task names kept for one signal row.
	MaxTaskRefsPerSignal = 5
	// MaxNamespacesPerDay bounds the namespaces kept for one day. Signals from
	// further namespaces still count in Prometheus but not in the summary.
	MaxNamespacesPerDay = 64
)

var categoryReasons = map[Category][]Reason{
	CategoryUnsupportedRequest: {
		ReasonRuntime, ReasonContractVersion, ReasonExecutionWorkspace, ReasonToolboxes,
		ReasonPriorTaskRef, ReasonMaxTurns, ReasonRetryPolicy, ReasonResources, ReasonPlacement,
		ReasonAutonomousCoordination, ReasonTransactionDelegation, ReasonNativeSession,
	},
	CategoryMissingTool: {ReasonToolNotRegistered, ReasonToolNotEnabled, ReasonToolDeniedByPolicy},
}

var runtimes = []string{
	RuntimeNative, RuntimeCodex, RuntimeClaude, RuntimeCopilot, RuntimeOpencode, RuntimeExternal,
}

// GatewayOwner identifies the Gateway that owns a Task. The zero value means
// the Task is not Gateway-owned. It is never a metric label; Summary uses it
// to leave out signals the caller may not read.
type GatewayOwner struct {
	Namespace    string
	NamespaceUID string
	Name         string
	UID          string
}

// Signal is one observation. Namespace and Task are Kubernetes object names;
// Tool is a candidate tool name that is replaced by ToolUnknown unless it is
// in the Recorder's catalog. No field may carry free text.
type Signal struct {
	Category  Category
	Reason    Reason
	Runtime   string
	Tool      string
	Namespace string
	Task      string
	// Gateway is the Task's owning Gateway, if any.
	Gateway GatewayOwner
}

// SignalSummary is one aggregated row of a Summary.
type SignalSummary struct {
	Category string   `json:"category"`
	Reason   string   `json:"reason"`
	Runtime  string   `json:"runtime"`
	Tool     string   `json:"tool"`
	Count    int64    `json:"count"`
	Tasks    []string `json:"tasks,omitempty"`
}

// Summary aggregates one namespace's signals over the window.
type Summary struct {
	Namespace string `json:"namespace"`
	// From is the start of the oldest UTC day in the window; To is now.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// RecordingSince is when this controller process started recording.
	// Partial is true when that is after From, so earlier days are missing.
	RecordingSince time.Time       `json:"recordingSince"`
	Partial        bool            `json:"partial"`
	Signals        []SignalSummary `json:"signals"`
}

type signalKey struct {
	category, reason, runtime, tool string
	gateway                         GatewayOwner
}

type signalEntry struct {
	count int64
	// tasks holds distinct Task names, most recent last.
	tasks []string
}

type dayBucket map[string]map[signalKey]*signalEntry

// Recorder keeps a bounded in-memory window of signals and increments the
// Prometheus counter. A nil Recorder ignores every call.
type Recorder struct {
	mu    sync.Mutex
	now   func() time.Time
	since time.Time
	tools map[string]string
	days  map[int64]dayBucket
}

// NewRecorder returns a Recorder whose tool labels are limited to catalog.
// Matching ignores case; the label is the catalog spelling.
func NewRecorder(catalog []string) *Recorder {
	return newRecorder(catalog, time.Now)
}

func newRecorder(catalog []string, now func() time.Time) *Recorder {
	tools := make(map[string]string, len(catalog))
	for _, name := range catalog {
		name = strings.TrimSpace(name)
		if name == "" || name == ToolUnknown || name == ToolNone {
			continue
		}
		if _, exists := tools[strings.ToLower(name)]; !exists {
			tools[strings.ToLower(name)] = name
		}
	}
	return &Recorder{now: now, since: now().UTC(), tools: tools, days: map[int64]dayBucket{}}
}

var defaultRecorder atomic.Pointer[Recorder]

// Default returns the installed Recorder, or nil when recording is off.
func Default() *Recorder {
	return defaultRecorder.Load()
}

// SetDefault installs r (nil turns recording off) and returns a function that
// restores the previous Recorder.
func SetDefault(r *Recorder) (restore func()) {
	previous := defaultRecorder.Swap(r)
	return func() { defaultRecorder.Store(previous) }
}

// Record records s on the installed Recorder, if any.
func Record(s Signal) {
	Default().Record(s)
}

// RuntimeLabel maps a runtime or provider kind to its label.
func RuntimeLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if slices.Contains(runtimes, value) {
		return value
	}
	return RuntimeUnknown
}

// Record counts s. Invalid categories are dropped; every other field is
// normalized to a closed value, and invalid object names are left out of the
// summary rather than recorded.
func (r *Recorder) Record(s Signal) {
	if r == nil {
		return
	}
	reasons, ok := categoryReasons[s.Category]
	if !ok {
		return
	}
	key := signalKey{
		category: string(s.Category),
		reason:   string(ReasonOther),
		runtime:  RuntimeLabel(s.Runtime),
		tool:     ToolNone,
	}
	if slices.Contains(reasons, s.Reason) {
		key.reason = string(s.Reason)
	}
	if s.Category == CategoryMissingTool {
		key.tool = r.toolLabel(s.Tool)
	}
	metrics.RecordCapabilityGapSignal(key.category, key.reason, key.runtime, key.tool)

	if len(validation.IsDNS1123Label(s.Namespace)) != 0 {
		return
	}
	// Callers may pass strings borrowed from a request buffer; the summary
	// keeps its own copies.
	namespace := strings.Clone(s.Namespace)
	task := strings.Clone(s.Task)
	if task != "" && len(validation.IsDNS1123Subdomain(task)) != 0 {
		task = ""
	}
	key.gateway = GatewayOwner{
		Namespace: strings.Clone(s.Gateway.Namespace), NamespaceUID: strings.Clone(s.Gateway.NamespaceUID),
		Name: strings.Clone(s.Gateway.Name), UID: strings.Clone(s.Gateway.UID),
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	today := dayNumber(now)
	r.pruneLocked(today)
	bucket := r.days[today]
	if bucket == nil {
		bucket = dayBucket{}
		r.days[today] = bucket
	}
	signals := bucket[namespace]
	if signals == nil {
		if len(bucket) >= MaxNamespacesPerDay {
			return
		}
		signals = map[signalKey]*signalEntry{}
		bucket[namespace] = signals
	}
	entry := signals[key]
	if entry == nil {
		entry = &signalEntry{}
		signals[key] = entry
	}
	entry.count++
	if task != "" {
		entry.tasks = appendRecentTask(entry.tasks, task)
	}
}

// GatewayOwners returns the distinct Gateways that own Tasks with signals in
// namespace's window, so a caller can decide which it may read before calling
// Summary.
func (r *Recorder) GatewayOwners(namespace string) []GatewayOwner {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(dayNumber(r.now().UTC()))
	seen := map[GatewayOwner]bool{}
	owners := []GatewayOwner{}
	for _, bucket := range r.days {
		for key := range bucket[namespace] {
			if key.gateway != (GatewayOwner{}) && !seen[key.gateway] {
				seen[key.gateway] = true
				owners = append(owners, key.gateway)
			}
		}
	}
	return owners
}

// Summary returns namespace's signals for the current UTC day and the
// WindowDays-1 days before it, largest count first. Signals from a
// Gateway-owned Task count only when visible reports that Gateway as readable;
// a nil visible leaves out every Gateway-owned signal. visible runs with the
// Recorder locked and must not block.
func (r *Recorder) Summary(namespace string, visible func(GatewayOwner) bool) Summary {
	if r == nil {
		return Summary{Namespace: namespace, Signals: []SignalSummary{}}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	today := dayNumber(now)
	r.pruneLocked(today)
	from := time.Unix((today-WindowDays+1)*secondsPerDay, 0).UTC()
	summary := Summary{
		Namespace:      namespace,
		From:           from,
		To:             now,
		RecordingSince: r.since,
		Partial:        r.since.After(from),
	}

	totals := map[signalKey]*signalEntry{}
	days := make([]int64, 0, len(r.days))
	for day := range r.days {
		days = append(days, day)
	}
	slices.Sort(days)
	for _, day := range days {
		for key, entry := range r.days[day][namespace] {
			if key.gateway != (GatewayOwner{}) && (visible == nil || !visible(key.gateway)) {
				continue
			}
			key.gateway = GatewayOwner{}
			total := totals[key]
			if total == nil {
				total = &signalEntry{}
				totals[key] = total
			}
			total.count += entry.count
			for _, task := range entry.tasks {
				total.tasks = appendRecentTask(total.tasks, task)
			}
		}
	}

	summary.Signals = make([]SignalSummary, 0, len(totals))
	for key, total := range totals {
		tasks := slices.Clone(total.tasks)
		slices.Reverse(tasks)
		summary.Signals = append(summary.Signals, SignalSummary{
			Category: key.category, Reason: key.reason, Runtime: key.runtime, Tool: key.tool,
			Count: total.count, Tasks: tasks,
		})
	}
	slices.SortFunc(summary.Signals, func(a, b SignalSummary) int {
		if a.Count != b.Count {
			if a.Count > b.Count {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Category+"/"+a.Reason+"/"+a.Runtime+"/"+a.Tool, b.Category+"/"+b.Reason+"/"+b.Runtime+"/"+b.Tool)
	})
	return summary
}

func (r *Recorder) toolLabel(name string) string {
	if canonical, ok := r.tools[strings.ToLower(strings.TrimSpace(name))]; ok {
		return canonical
	}
	return ToolUnknown
}

func (r *Recorder) pruneLocked(today int64) {
	for day := range r.days {
		if day <= today-WindowDays || day > today {
			delete(r.days, day)
		}
	}
}

const secondsPerDay = 24 * 60 * 60

func dayNumber(t time.Time) int64 {
	return t.Unix() / secondsPerDay
}

// appendRecentTask moves task to the end and keeps at most
// MaxTaskRefsPerSignal names.
func appendRecentTask(tasks []string, task string) []string {
	tasks = slices.DeleteFunc(tasks, func(existing string) bool { return existing == task })
	tasks = append(tasks, task)
	if len(tasks) > MaxTaskRefsPerSignal {
		tasks = slices.Delete(tasks, 0, len(tasks)-MaxTaskRefsPerSignal)
	}
	return tasks
}
