package storagetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The usage and memory write cases.

var (
	catalogUsageWorkID = storagev1.UsageWorkID(workflowNamespace, "monitor-uid", "org/repo", "issue", 1)
	// catalogUsagePruneCutoff is fixed once, so every attempt of the prune
	// case sends the same request.
	catalogUsagePruneCutoff = time.Now().UTC().Add(-90 * 24 * time.Hour).Truncate(time.Second)
)

func catalogUsageWork(startedAt time.Time) storagev1.UsageWorkRequest {
	return storagev1.UsageWorkRequest{Namespace: workflowNamespace, MonitorName: workflowMonitor, MonitorUID: "monitor-uid",
		Repository: "Org/Repo", Kind: "issue", Number: 1, StartedAt: startedAt}
}

func catalogUsageTask(phase string, at time.Time) storagev1.UsageTask {
	return storagev1.UsageTask{Namespace: workflowNamespace, TaskUID: "task-1", TaskName: "task-1", WorkID: catalogUsageWorkID,
		Phase: phase, StartedAt: at, PhaseObservedAt: at}
}

func catalogUsageLink() storagev1.UsagePRLink {
	return storagev1.UsagePRLink{Namespace: workflowNamespace, WorkID: catalogUsageWorkID, Repository: "org/repo", Number: 7,
		Origin: "assisted", EvidenceID: "evidence-1", LinkedAt: workflowAt(5)}
}

func registerCatalogUsageWork(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	require.NoError(t, p.RegisterUsageWork(t.Context(), catalogUsageWork(workflowAt(0))))
}

func observeUsage(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	data, err := p.LoadUsage(t.Context(), storagev1.UsageFilter{Namespaces: []string{workflowNamespace}, AsOf: time.Now().UTC()})
	require.NoError(t, err)
	return data
}

func observeUsageObservations(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	observations, err := p.ListTaskCounterObservations(t.Context(), workflowNamespace, "task-1")
	require.NoError(t, err)
	return observations
}

func catalogMemory(content string) *storagev1.Memory {
	return &storagev1.Memory{ID: "mem-1", Namespace: memoryTenant, Source: "task", Content: content, Tags: []string{"ops"}}
}

func catalogMemoryProposal() *storagev1.MemoryProposal {
	return &storagev1.MemoryProposal{ID: "mprop-1", Namespace: memoryTenant, Type: storagev1.MemoryProposalTypeMemory,
		Title: "Fact", Content: "The cache is warm"}
}

func observeMemories(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	memories, err := p.ListMemories(t.Context(), storagev1.MemoryFilter{Namespace: memoryTenant, IncludeDisabled: true, IncludeDeleted: true})
	require.NoError(t, err)
	return memories
}

func observeMemoryProposals(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	proposals, err := p.ListMemoryProposals(t.Context(), storagev1.MemoryProposalFilter{Namespace: memoryTenant})
	require.NoError(t, err)
	proposal, getErr := p.GetMemoryProposal(t.Context(), memoryTenant, "mprop-1")
	return []any{proposals, observed(t, proposal, getErr)}
}

func init() {
	type usageCase = writeCase[storagev1.StateProvider]
	stateWriteCases = append(stateWriteCases,
		usageCase{
			method:  "RegisterUsageWork",
			args:    fixed(catalogUsageWork(workflowAt(0))),
			observe: observeUsage,
		},
		usageCase{
			method:  "RegisterUsageTask",
			setup:   registerCatalogUsageWork,
			args:    fixed(catalogUsageTask("Running", workflowAt(1))),
			observe: observeUsage,
		},
		usageCase{
			method: "RecordUsage",
			args: rebuiltArgs(func() []any {
				input, output := int64(100), int64(10)
				return []any{storagev1.UsageObservation{ID: "observation-1", Namespace: workflowNamespace, TaskUID: "task-1",
					TaskName: "task-1", CounterID: "session-1", Scope: "session", Source: "agent", Model: "model",
					InputTokens: &input, OutputTokens: &output, Status: "running", ObservedAt: workflowAt(0)}}
			}),
			observe: observeUsageObservations,
		},
		usageCase{
			method:  "LinkUsagePullRequest",
			setup:   registerCatalogUsageWork,
			args:    fixed(catalogUsageLink()),
			observe: observeUsage,
		},
		usageCase{
			method: "RecordUsagePullRequest",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				registerCatalogUsageWork(t, p)
				require.NoError(t, p.LinkUsagePullRequest(t.Context(), catalogUsageLink()))
			},
			args: fixed(storagev1.UsagePullRequest{Namespace: workflowNamespace, Repository: "org/repo", Number: 7,
				GitHubID: "PR_7", URL: "https://forge.example/pr/7", State: "open", ObservedAt: workflowAt(6)}),
			observe: observeUsage,
		},
		usageCase{
			method: "PruneUsage",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				start := catalogUsagePruneCutoff.Add(-30 * 24 * time.Hour)
				require.NoError(t, p.RegisterUsageWork(t.Context(), catalogUsageWork(start)))
				require.NoError(t, p.RegisterUsageTask(t.Context(), catalogUsageTask("Failed", start)))
			},
			args:    fixed(catalogUsagePruneCutoff),
			observe: observeUsage,
		},
		usageCase{
			method:  "CreateMemory",
			args:    rebuiltArgs(func() []any { return []any{&storagev1.Memory{Namespace: memoryTenant, Content: "remember this"}} }),
			observe: observeMemories,
		},
		usageCase{
			method: "UpdateMemory",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateMemory(t.Context(), catalogMemory("first")))
			},
			args:    rebuiltArgs(func() []any { return []any{catalogMemory("updated")} }),
			observe: observeMemories,
		},
		usageCase{
			method: "DeleteMemory",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateMemory(t.Context(), catalogMemory("first")))
			},
			args:    fixed(memoryTenant, "mem-1"),
			observe: observeMemories,
		},
		usageCase{
			method: "SetMemoryDisabled",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateMemory(t.Context(), catalogMemory("first")))
			},
			args:    fixed(memoryTenant, "mem-1", true),
			observe: observeMemories,
		},
		usageCase{
			method: "CreateMemoryProposal",
			args: rebuiltArgs(func() []any {
				return []any{&storagev1.MemoryProposal{Namespace: memoryTenant, Title: "Retry skill", SkillName: "retry",
					Content: "Back off exponentially"}}
			}),
			observe: observeMemoryProposals,
		},
		usageCase{
			method: "ReviewMemoryProposal",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateMemoryProposal(t.Context(), catalogMemoryProposal()))
			},
			args: fixed(storagev1.MemoryProposalReview{Namespace: memoryTenant, ID: "mprop-1",
				Status: storagev1.MemoryProposalStatusAccepted, Reviewer: "reviewer", ReviewNote: "looks right"}),
			observe: observeMemoryProposals,
		},
		usageCase{
			method: "ArchiveMemoryProposal",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateMemoryProposal(t.Context(), catalogMemoryProposal()))
			},
			args:    fixed(memoryTenant, "mprop-1"),
			observe: observeMemoryProposals,
		},
	)
}
