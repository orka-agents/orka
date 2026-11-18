package applicationexec

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
)

func (b *Backend) qualifySource(ctx context.Context, r *run, side string) (bool, error) {
	boundaryKey := side + "/source"
	if digestPattern.MatchString(r.state.Boundary[boundaryKey]) {
		return true, nil
	}
	target, err := b.inputTarget(r, side)
	if err != nil {
		return false, err
	}
	taskUID := string(r.state.Task.UID)
	if !r.state.Qualification {
		taskUID = string(r.state.ArmTasks[side].UID)
	}
	var proofs []pv.ApplicationBoundaryReport
	for index, endpoint := range b.config.SourceEndpoints {
		for _, stage := range []struct {
			name, role, account string
			positive            bool
		}{{"before", rolePrepare, roleWorker, true}, {"denied", roleSubject, roleSubject, false}, {"after", rolePrepare, roleWorker, true}} {
			role := "source-" + strconv.Itoa(index) + "-" + stage.name
			key := resourceKey(side, role)
			input := pv.ApplicationBoundaryInput{Version: pv.ApplicationProtocolVersion,
				ID:      "source-" + strings.TrimPrefix(pv.Digest([]byte(key)), "sha256:")[:24],
				TaskUID: taskUID, Target: target, CanaryHost: endpoint.Address, CanaryPort: 443,
				SourceHost: endpoint.Host, PositiveControl: stage.positive}
			if stage.positive {
				input.FileSizeRole = workerModePrepare
			}
			if err := b.boundaryRuntime(&input); err != nil {
				return false, err
			}
			report, recorded := r.state.BoundaryReports[key]
			if !recorded {
				job, err := b.ensureJob(ctx, r, side, role, input, jobOptions{role: stage.role, mode: workerModeBoundary,
					image: b.config.WorkerImage, taskUID: taskUID, serviceAccount: stage.account, deadline: 45, fileSizeRole: input.FileSizeRole})
				if err != nil {
					return false, err
				}
				pod, _, ready, err := b.observeJob(ctx, r, side, role, job, b.config.WorkerImageID, true)
				if err != nil || !ready {
					return false, err
				}
				report, err = readWorkerReport(ctx, b, pod, pv.MaxApplicationReportBytes, pv.DecodeApplicationBoundaryReport)
				if err != nil {
					return false, err
				}
				if report.PodUID != string(pod.UID) {
					return false, contract.ErrConflict
				}
			}
			if pv.ValidateApplicationBoundaryReport(input, report) != nil {
				return false, ErrIsolation
			}
			if !recorded {
				r.state.BoundaryReports[key] = report
				if err := b.save(ctx, r); err != nil {
					return false, err
				}
			}
			proofs = append(proofs, report)
		}
	}
	if len(proofs) == 0 {
		return false, ErrIsolation
	}
	content, _ := json.Marshal(struct {
		Target pv.ApplicationTarget
		Node   targetIdentity
		Proofs []pv.ApplicationBoundaryReport
	}{target, r.state.Target, proofs})
	r.state.Boundary[boundaryKey] = pv.Digest(content)
	return true, b.save(ctx, r)
}
