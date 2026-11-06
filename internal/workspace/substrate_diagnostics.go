package workspace

import (
	"context"
	"fmt"
	"time"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type SubstrateDiagnosticCheck struct {
	Area   string `json:"area"`
	Ready  bool   `json:"ready"`
	Reason string `json:"reason"`
}

type SubstrateDiagnostics struct {
	Ready                        bool                       `json:"ready"`
	Checks                       []SubstrateDiagnosticCheck `json:"checks"`
	AdapterCapabilities          []string                   `json:"adapterCapabilities"`
	NativeLifecyclePreconditions bool                       `json:"nativeLifecyclePreconditions"`
}

// DiagnoseSubstrate is read-only. It reports configuration and observed native
// API compatibility separately from the adapter's implemented capabilities.
// Execution and router stream timeouts require the bundled conformance suite.
func DiagnoseSubstrate(ctx context.Context, cfg SubstrateConfig, templateName string) SubstrateDiagnostics {
	report := SubstrateDiagnostics{Ready: true, AdapterCapabilities: []string{"direct-exec", "mcp-actors", "acp-runtime", "data-only-suspend", "checkpoint-export", "cold-restore", "explicit-recovery"}}
	check := func(area string, ready bool, reason string) {
		report.Checks = append(report.Checks, SubstrateDiagnosticCheck{Area: area, Ready: ready, Reason: reason})
		report.Ready = report.Ready && ready
	}
	api, err := NewSubstrateNativeClient(cfg)
	if err != nil {
		check("configuration", false, "configure a trusted TLS endpoint and exactly one of mTLS or bearer-file authentication")
		return report
	}
	defer api.Close() //nolint:errcheck
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = api.Control.GetAtespace(ctx, &ateapipb.GetAtespaceRequest{Atespace: &ateapipb.ObjectRef{Name: cfg.Atespace}})
	if err != nil {
		reason := "native API connectivity failed; check endpoint, server trust, and network access"
		switch status.Code(err) {
		case codes.Unauthenticated:
			reason = "control identity was rejected; check certificate or bearer rotation"
		case codes.PermissionDenied:
			reason = "control identity cannot read the selected Atespace"
		case codes.NotFound:
			reason = "selected native Atespace does not exist"
		case codes.Unimplemented:
			reason = "server does not implement the supported native ateapi.Control protocol"
		}
		check("control-api", false, reason)
		return report
	}
	check("control-api", true, "authenticated native Atespace read succeeded")
	if _, err := api.ListTags(ctx, cfg.Atespace); err != nil {
		check("tags", false, "immutable Tag inventory is unavailable; check native API version and Tag permissions")
	} else {
		check("tags", true, "native Tag inventory is readable")
	}
	if templateName == "" {
		check("template", false, "select an infrastructure template to verify gVisor and snapshot storage")
		return report
	}
	template, err := api.Control.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{ActorTemplate: &ateapipb.ObjectRef{Atespace: cfg.Atespace, Name: templateName}})
	if err != nil {
		check("template", false, "native ActorTemplate is unavailable; Kubernetes ActorTemplate CRDs are not used")
		return report
	}
	snapshot := template.GetSnapshotConfig()
	check("data-only-snapshots", snapshot.GetPreferredFidelity() == ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES &&
		snapshot.GetGoldenSnapshotConfig().GetMode() == ateapipb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED,
		"ACP requires snapshotConfig preferredFidelity VOLUMES with golden snapshots disabled, so process memory is never captured or restored")
	selector := template.GetWorkerSelector().GetMatchLabels()
	check("placement", len(selector) != 0, "ACP requires an explicit selector identifying one WorkerPool")
	limits := template.GetResources().GetLimits()
	pages := substratePages{}
	count := 0
	for {
		workers, err := api.Control.ListWorkers(ctx, &ateapipb.ListWorkersRequest{Options: pages.options()})
		if err != nil {
			check("workers", false, "native worker inventory is unavailable")
			return report
		}
		for _, worker := range workers.GetWorkers() {
			matches := len(selector) != 0
			for key, value := range selector {
				matches = matches && worker.GetLabels()[key] == value
			}
			workerStatus := worker.GetStatus()
			if matches && workerStatus.GetState() == ateapipb.WorkerState_WORKER_STATE_ACTIVE &&
				substrateWorkerRunsGVisor(workerStatus.GetSandboxClasses()) && substrateWorkerFitsExactlyOne(workerStatus.GetCapacity(), limits) {
				count++
			}
		}
		more, err := pages.advance(workers.GetNextPageToken())
		if err != nil {
			check("workers", false, "native worker pagination repeated a token")
			return report
		}
		if !more {
			break
		}
	}
	check("workers", count > 0, fmt.Sprintf("%d matching active gVisor workers are sized for exactly one template-sized Actor; worker Pods are separate from Actor scale-to-zero", count))
	return report
}

// Substrate v1 workers report the gVisor class through their registered
// sandbox runtimes; templates no longer carry a sandbox class.
func substrateWorkerRunsGVisor(classes []*ateapipb.WorkerSandboxClass) bool {
	if len(classes) == 0 {
		return false
	}
	for _, class := range classes {
		if class.GetSandboxClass() != "gvisor" {
			return false
		}
	}
	return true
}

// Upstream workers accept up to --max-actors Actors, which no WorkerPool field
// sets. Placement books template limits against worker capacity, so a worker
// whose CPU or memory cannot hold a second template-sized Actor hosts one.
func substrateWorkerFitsExactlyOne(capacity *ateapipb.WorkerResources, limits *ateapipb.Resources) bool {
	cpu, memory := limits.GetCpuMillicores(), limits.GetMemoryBytes()
	if cpu <= 0 || memory <= 0 || capacity.GetActors() < 1 || capacity.GetCpuMillicores() < cpu || capacity.GetMemoryBytes() < memory {
		return false
	}
	return capacity.GetActors() == 1 || capacity.GetCpuMillicores() < 2*cpu || capacity.GetMemoryBytes() < 2*memory
}
