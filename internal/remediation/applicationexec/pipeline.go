package applicationexec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func (b *Backend) advanceQualification(ctx context.Context, r *run) (bool, error) {
	if !r.state.Qualification || r.state.Cancelled || r.state.Failed != "" {
		return false, contract.ErrConflict
	}
	if r.state.Finished {
		return b.cleanup(ctx, r)
	}
	if err := b.ensureClaim(ctx, r); err != nil {
		return false, err
	}
	if err := b.ensureSideInfrastructure(ctx, r, "qualification", false); err != nil {
		return false, err
	}
	done, err := b.qualifyBoundary(ctx, r, "qualification")
	if err != nil || !done {
		return false, err
	}
	done, err = b.qualifySource(ctx, r, "qualification")
	if err != nil || !done {
		return false, err
	}
	r.state.Finished = true
	if err := b.save(ctx, r); err != nil {
		return false, err
	}
	return false, nil
}

func (b *Backend) qualifyBoundary(ctx context.Context, r *run, side string) (bool, error) {
	if digestPattern.MatchString(r.state.Boundary[side]) {
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
	proofs := make([]pv.ApplicationBoundaryReport, 0, 3)
	for _, step := range []struct {
		name, role, sa string
		positive       bool
	}{{"positive-before", rolePositive, roleObserver, true}, {"denied", roleSubject, roleSubject, false}, {"positive-after", rolePositive, roleObserver, true}} {
		key := resourceKey(side, "boundary-"+step.name)
		input := pv.ApplicationBoundaryInput{Version: pv.ApplicationProtocolVersion,
			ID:      "boundary-" + strings.TrimPrefix(pv.Digest([]byte(key)), "sha256:")[:24],
			TaskUID: taskUID, Target: target, CanaryHost: b.config.CanaryHost, CanaryPort: b.config.CanaryPort,
			PositiveControl: step.positive, APIAddress: b.config.TargetAPIServer, ServiceAccountName: "orka-" + step.sa}
		if err := b.boundaryRuntime(&input); err != nil {
			return false, err
		}
		report, recorded := r.state.BoundaryReports[key]
		if !recorded {
			job, err := b.ensureJob(ctx, r, side, "boundary-"+step.name, input, jobOptions{
				role: step.role, mode: workerModeBoundary, image: b.config.WorkerImage, taskUID: taskUID,
				serviceAccount: step.sa, token: true, deadline: 45})
			if err != nil {
				return false, err
			}
			pod, _, ready, err := b.observeJob(ctx, r, side, "boundary-"+step.name, job, b.config.WorkerImageID, true)
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
			r.state.BoundaryReports[key] = report
		}
		sa := r.effect(resourceKey(side, "sa-"+step.sa))
		if sa == nil || string(sa.UID) != report.APIUserUID || pv.ValidateApplicationBoundaryReport(input, report) != nil {
			return false, ErrIsolation
		}
		if !recorded {
			if err := b.save(ctx, r); err != nil {
				return false, err
			}
		}
		proofs = append(proofs, report)
	}
	content, _ := json.Marshal(struct {
		Target pv.ApplicationTarget
		Node   targetIdentity
		Proofs []pv.ApplicationBoundaryReport
	}{target, r.state.Target, proofs})
	r.state.Boundary[side] = pv.Digest(content)
	return true, b.save(ctx, r)
}

func (b *Backend) advanceApplication(ctx context.Context, r *run) (bool, error) {
	for _, side := range pv.ActionSides(r.state.Request.Action) {
		if err := b.ensureTask(ctx, r, side); err != nil {
			return false, err
		}
	}
	if err := b.ensureClaim(ctx, r); err != nil {
		return false, err
	}
	for _, side := range pv.ActionSides(r.state.Request.Action) {
		if err := b.ensureSideInfrastructure(ctx, r, side, true); err != nil {
			return false, err
		}
		if err := b.ensureProtected(ctx, r, side); err != nil {
			return false, err
		}
		if err := b.ensureSubjectTLS(ctx, r, side); err != nil {
			return false, err
		}
		done, err := b.qualifyBoundary(ctx, r, side)
		if err != nil || !done {
			return false, err
		}
		done, err = b.qualifySource(ctx, r, side)
		if err != nil || !done {
			return false, err
		}
	}
	for _, side := range pv.ActionSides(r.state.Request.Action) {
		done, err := b.prepare(ctx, r, side)
		if err != nil || !done {
			return false, err
		}
	}
	for _, side := range pv.ActionSides(r.state.Request.Action) {
		if side == pv.Patched {
			record, err := b.store.GetPatchVerificationRun(ctx, r.state.Binding.RunID)
			if err != nil {
				return false, safeError(contract.ErrUnavailable, err)
			}
			observations := make([]pv.Observation, 0, len(record.Evidence))
			for _, evidence := range record.Evidence {
				if evidence.Rejection != "" {
					return false, contract.ErrConflict
				}
				observations = append(observations, evidence.Observation)
			}
			if !pv.OriginalReady(*r.state.Manifest, *r.state.Binding, observations) {
				_, err := b.store.FinalizePatchVerificationRun(ctx, *r.state.Binding)
				return err == nil, safeErrorOrNil(err)
			}
		}
		done, err := b.executeSide(ctx, r, side)
		if err != nil || !done {
			return false, err
		}
	}
	_, err := b.store.FinalizePatchVerificationRun(ctx, *r.state.Binding)
	return err == nil, safeErrorOrNil(err)
}

func safeErrorOrNil(err error) error {
	if err == nil {
		return nil
	}
	return safeError(contract.ErrUnavailable, err)
}

func (b *Backend) prepare(ctx context.Context, r *run, side string) (bool, error) {
	if slices.Contains(r.state.Prepared, side) {
		return true, nil
	}
	input := b.preparationInput(r.state.Request, string(r.state.ArmTasks[side].UID), r)
	job, err := b.ensureJob(ctx, r, side, workerModePrepare, input, jobOptions{role: rolePrepare, mode: workerModePrepare,
		image: b.config.WorkerImage, taskUID: input.TaskUID, serviceAccount: roleWorker, source: true, deadline: 600})
	if err != nil {
		return false, err
	}
	pod, _, ready, err := b.observeJob(ctx, r, side, workerModePrepare, job, b.config.WorkerImageID, true)
	if err != nil || !ready {
		return false, err
	}
	report, err := readWorkerReport(ctx, b, pod, pv.MaxApplicationPreparationReportBytes, pv.DecodeApplicationPreparationReport)
	if err != nil {
		return false, err
	}
	if report.PodUID != string(pod.UID) || pv.ValidateApplicationPreparationReport(input, report) != nil {
		return false, contract.ErrConflict
	}
	if r.state.Manifest == nil {
		r.state.Manifest = &report.Manifest
		patchedUID := ""
		if r.state.Request.Action == pv.VerifyPatch {
			patchedUID = string(r.state.ArmTasks[pv.Patched].UID)
		}
		binding, err := pv.NewRunBinding(report.Manifest, string(r.state.Task.UID),
			string(r.state.ArmTasks[pv.Original].UID), patchedUID)
		if err != nil {
			return false, contract.ErrConflict
		}
		r.state.Binding = &binding
		if err := b.save(ctx, r); err != nil {
			return false, err
		}
	}
	if !reflect.DeepEqual(*r.state.Manifest, report.Manifest) || r.state.Binding == nil {
		return false, contract.ErrConflict
	}
	// The existing store validates complete provenance and treats an exact
	// repeated creation as idempotent. A crash after commit cannot create a new
	// attempt or silently change the original/candidate source.
	if !r.state.RecordCreated {
		if err := b.store.CreatePatchVerificationRun(ctx, report.Manifest, *r.state.Binding, report.Provenance); err != nil {
			return false, safeError(contract.ErrUnavailable, err)
		}
		r.state.RecordCreated = true
	}
	r.state.Prepared = append(r.state.Prepared, side)
	return true, b.save(ctx, r)
}

func (b *Backend) executeSide(ctx context.Context, r *run, side string) (bool, error) {
	built, err := b.buildSide(ctx, r, side)
	if err != nil || !built {
		return false, err
	}
	replicaReady, err := b.ensureReplica(ctx, r, side)
	if err != nil || !replicaReady {
		return false, err
	}
	plan := r.state.Request.Checks.Application
	if err := b.ensureFixtures(ctx, r, side, "resource", plan.Resources); err != nil {
		return false, err
	}
	fixtures, ready, err := b.markers(ctx, r, side)
	if err != nil || !ready {
		return false, err
	}
	artifact := r.state.Artifacts[side]
	subjectInput, err := b.workerInput(r, side, "", workerModeSubject)
	if err != nil {
		return false, err
	}
	subjectInput.Artifact = &artifact
	subjectInput.SubjectTLS, err = b.subjectTLSIdentities(ctx, r, side)
	if err != nil {
		return false, err
	}
	aliases, err := b.serviceAliases(ctx, r, side)
	if err != nil {
		return false, err
	}
	protectedAliases, err := b.fixtureAliases(ctx, r, side)
	if err != nil {
		return false, err
	}
	aliases = append(aliases, protectedAliases...)
	subjectJob, err := b.ensureJob(ctx, r, side, workerModeSubject, subjectInput, jobOptions{
		role: roleSubject, mode: workerModeSubject, image: plan.Runtime.Image, taskUID: subjectInput.TaskUID,
		serviceAccount: roleSubject, token: len(plan.Permissions) != 0, inClusterToken: true,
		artifacts: true, artifactReadOnly: true, target: subjectTarget, hostAliases: aliases, subjectTLS: subjectInput.SubjectTLS})
	if err != nil {
		return false, err
	}
	subjectPod, subject, ready, err := b.observeJob(ctx, r, side, workerModeSubject, subjectJob, plan.Runtime.ImageID, false)
	if err != nil || !ready {
		return false, err
	}
	for index, check := range r.state.Request.Checks.Checks {
		done, err := b.executeCheck(ctx, r, side, index, check, fixtures, subjectPod, subject)
		if err != nil || !done {
			return false, err
		}
	}
	return true, nil
}

func (b *Backend) buildSide(ctx context.Context, r *run, side string) (bool, error) {
	if _, ready := r.state.Artifacts[side]; !ready {
		input, err := b.workerInput(r, side, "", workerModeBuild)
		if err != nil {
			return false, err
		}
		job, err := b.ensureJob(ctx, r, side, workerModeBuild, input, jobOptions{role: roleBuild, mode: workerModeBuild,
			image: b.config.ToolchainImage, taskUID: input.TaskUID, serviceAccount: roleWorker,
			source: true, artifacts: true, deadline: 600})
		if err != nil {
			return false, err
		}
		pod, identity, ready, err := b.observeJob(ctx, r, side, workerModeBuild, job, b.config.ToolchainImageID, true)
		if err != nil || !ready {
			return false, err
		}
		report, err := readWorkerReport(ctx, b, pod, pv.MaxApplicationReportBytes, pv.DecodeApplicationBuildReport)
		if err != nil {
			return false, err
		}
		artifact, err := pv.BindApplicationBuildReport(input, report, identity)
		if err != nil {
			return false, contract.ErrConflict
		}
		r.state.Artifacts[side] = artifact
		if err := b.save(ctx, r); err != nil {
			return false, err
		}
	}
	if side == pv.Patched && r.state.Artifacts[pv.Original].BinaryDigest == r.state.Artifacts[pv.Patched].BinaryDigest {
		return false, errors.New("identical application binaries cannot establish a changed execution outcome")
	}
	return true, nil
}

func (b *Backend) executeCheck(ctx context.Context, r *run, side string, index int, check pv.Check,
	fixtures []pv.ApplicationEndpoint, subjectPod *corev1.Pod, subject pv.ApplicationContainerIdentity) (bool, error) {
	artifact := r.state.Artifacts[side]
	slot := side + ":" + check.ID
	role := "observe-" + strconv.Itoa(index)
	physicalSide := observerSide(r, side)
	if slices.Contains(r.state.Observed, slot) {
		stopped, err := b.stopObserver(ctx, r, side, role)
		if err != nil || !stopped {
			return false, err
		}
		return true, nil
	}
	if err := b.ensureFixtures(ctx, r, side, "check-"+strconv.Itoa(index), check.Application.Resources); err != nil {
		return false, err
	}
	input, err := b.workerInput(r, side, check.ID, workerModeObserve)
	if err != nil {
		return false, err
	}
	input.Artifact, input.Subject = &artifact, &subject
	input.SubjectTLS = slices.Clone(r.state.SubjectTLS[side])
	input.Endpoints = slices.Clone(fixtures)
	if err := b.bindTCPInput(ctx, r, side, check, subjectPod, subject, &input); err != nil {
		return false, err
	}
	if len(check.Application.Actions) != 0 {
		input.Actors, err = b.actorIdentities(r, side, check)
		if err != nil {
			return false, err
		}
		input.ActionTargets, err = b.reserveActions(ctx, r, side, role, check, input.Actors)
		if err != nil {
			return false, err
		}
	}
	options := jobOptions{role: roleObserver, mode: workerModeObserve,
		image: b.config.WorkerImage, taskUID: input.TaskUID, serviceAccount: roleObserver, token: true,
		marker:   check.Application.TCP != nil && r.state.Request.ApplicationPolicy == nil,
		deadline: int64(check.TimeoutSeconds + 15), actors: input.Actors, actorSecret: actorSecretName(role)}
	if err := b.bindReceiverInput(ctx, r, physicalSide, check, &input, &options); err != nil {
		return false, err
	}
	return b.runObserver(ctx, r, side, role, slot, input, options)
}

func (b *Backend) bindTCPInput(ctx context.Context, r *run, side string, check pv.Check, subjectPod *corev1.Pod,
	subject pv.ApplicationContainerIdentity, input *pv.ApplicationWorkerInput) error {
	var err error
	if check.Application.TCP != nil {
		if r.state.Request.ApplicationPolicy != nil {
			input.MarkerRef, err = b.markerReference(ctx, r, side, check.Application.TCP.MarkerID)
			if err != nil {
				return err
			}
			input.MarkerDigest = input.MarkerRef.Digest
		} else {
			object, err := b.boundObject(ctx, r, resourceKey(side, "marker-secret"))
			if err != nil {
				return err
			}
			input.MarkerDigest = pv.Digest(object.(*corev1.Secret).Data[legacyMarkerKey])
		}
	}
	if tcp := check.Application.TCP; tcp != nil && tcp.Mode != receiveMode && tcp.Target == subjectTarget {
		if net.ParseIP(subjectPod.Status.PodIP) == nil {
			return contract.ErrConflict
		}
		input.Endpoints = append(input.Endpoints, pv.ApplicationEndpoint{ID: subjectTarget, Address: subjectPod.Status.PodIP,
			Port: tcp.Port, Pod: subject})
	}
	return nil
}

func (b *Backend) bindReceiverInput(ctx context.Context, r *run, physicalSide string, check pv.Check,
	input *pv.ApplicationWorkerInput, options *jobOptions) error {
	if tcp := check.Application.TCP; tcp != nil && tcp.Mode == receiveMode {
		object, err := b.boundObject(ctx, r, resourceKey(physicalSide, "fixture-service-"+tcp.Target))
		if err != nil {
			return err
		}
		service := object.(*corev1.Service)
		namespace := r.effect(resourceKey(physicalSide, "namespace"))
		if namespace == nil {
			return contract.ErrConflict
		}
		input.ReceiverService = &pv.ApplicationServiceIdentity{Namespace: service.Namespace, NamespaceUID: string(namespace.UID),
			Name: service.Name, UID: string(service.UID), Port: tcp.Port, Selector: service.Spec.Selector}
		options.receiverPort, options.target = tcp.Port, tcp.Target
	}
	return nil
}

func (b *Backend) runObserver(ctx context.Context, r *run, side, role, slot string, input pv.ApplicationWorkerInput, options jobOptions) (bool, error) {
	physicalSide := observerSide(r, side)
	job, err := b.ensureJob(ctx, r, physicalSide, role, input, options)
	if err != nil {
		return false, err
	}
	pod, observer, ready, err := b.observeJob(ctx, r, physicalSide, role, job, b.config.WorkerImageID, false, true)
	if err != nil {
		return false, err
	}
	if pod != nil {
		if err := b.ensureActorCredentials(ctx, r, side, role, pod, input.Actors); err != nil {
			return false, err
		}
	}
	if !ready {
		return false, nil
	}
	if input.ReceiverService != nil {
		ready, err := b.ensureReceiverRoute(ctx, r, side, role, *input.ReceiverService, observer)
		if err != nil || !ready {
			return false, err
		}
	}
	report, complete, err := b.observerTranscript(ctx, r, side, role, pod, input)
	if err != nil || !complete {
		return false, err
	}
	if err := b.enrichReplicaEvidence(ctx, r, side, &report.Evidence.Observation); err != nil {
		return false, err
	}
	if err := b.validateReport(ctx, r, input, observer, *report); err != nil {
		return false, err
	}
	if err := b.store.RecordPatchVerificationEvidence(ctx, *r.state.Binding, report.Evidence); err != nil {
		return false, safeError(contract.ErrUnavailable, err)
	}
	if err := b.recordObserved(ctx, r, slot); err != nil {
		return false, err
	}
	stopped, err := b.stopObserver(ctx, r, side, role)
	if err != nil || !stopped {
		return false, err
	}
	return true, nil
}

func (b *Backend) markers(ctx context.Context, r *run, side string) ([]pv.ApplicationEndpoint, bool, error) {
	if r.state.Request.ApplicationPolicy != nil {
		return b.protectedFixtures(ctx, r, side)
	}
	fixtures := r.state.Request.Checks.Application.Fixtures
	if len(fixtures) == 0 {
		return nil, true, nil
	}
	if r.effect(resourceKey(side, "marker-secret")) == nil {
		value := make([]byte, 32)
		if _, err := rand.Read(value); err != nil {
			return nil, false, contract.ErrUnavailable
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: markerSecretName, Namespace: namespaceName(r, side)},
			Immutable: new(true), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{legacyMarkerKey: []byte(hex.EncodeToString(value))}}
		if _, err := b.ensureObject(ctx, r, resourceKey(side, "marker-secret"), secret); err != nil {
			return nil, false, err
		}
	}
	object, err := b.boundObject(ctx, r, resourceKey(side, "marker-secret"))
	if err != nil {
		return nil, false, err
	}
	secret := object.(*corev1.Secret)
	if !ptr.Deref(secret.Immutable, false) || len(secret.Data) != 1 || len(secret.Data[legacyMarkerKey]) != 64 {
		return nil, false, contract.ErrConflict
	}
	jobs := make([]fixtureJob, 0, len(fixtures))
	for _, fixture := range fixtures {
		role := "marker-" + fixture.ID
		job, err := b.ensureJob(ctx, r, side, role, struct{}{}, jobOptions{role: roleMarker, mode: workerModeMarker,
			image: b.config.WorkerImage, taskUID: string(r.state.ArmTasks[side].UID),
			serviceAccount: roleWorker, marker: true, markerPort: fixture.Port, target: fixture.ID})
		if err != nil {
			return nil, false, err
		}
		jobs = append(jobs, fixtureJob{fixture: fixture, role: role, imageID: b.config.WorkerImageID, job: job})
	}
	return b.observeFixtures(ctx, r, side, jobs)
}

func (b *Backend) protectedFixtures(ctx context.Context, r *run, side string) ([]pv.ApplicationEndpoint, bool, error) {
	jobs := make([]fixtureJob, 0, len(r.state.Request.Checks.Application.Fixtures))
	physicalSide := protectedSide(side)
	for _, fixture := range r.state.Request.Checks.Application.Fixtures {
		if fixture.Kind == "receiver" || fixture.Kind == fixtureKindReplica {
			continue
		}
		mode, image, imageID, account := workerModeMarker, b.config.WorkerImage, b.config.WorkerImageID, roleMarker
		var profile *pv.ApplicationFixtureProfile
		if fixture.Kind == fixtureKindImage {
			for i := range r.state.Request.ApplicationPolicy.Fixtures {
				candidate := &r.state.Request.ApplicationPolicy.Fixtures[i]
				if candidate.ID == fixture.Profile {
					profile = candidate
				}
			}
			if profile == nil {
				return nil, false, contract.ErrInvalid
			}
			mode, image, imageID, account = workerModeFixture, profile.Image, profile.ImageID, roleWorker
		}
		input, err := b.workerInput(r, side, "", mode)
		if err != nil {
			return nil, false, err
		}
		input.FixtureID, input.Worker.ImageID = fixture.ID, imageID
		role := "fixture-" + fixture.ID
		options := jobOptions{role: mode, mode: mode, image: image, taskUID: input.TaskUID,
			serviceAccount: account, markerPort: fixture.Port, markerInput: true, target: fixture.ID, receiverPort: fixture.Port}
		if mode == workerModeMarker {
			input.MarkerRef, err = b.markerReference(ctx, r, side, fixture.ID)
			if err != nil {
				return nil, false, err
			}
			input.MarkerDigest, options.token = input.MarkerRef.Digest, true
		} else {
			var data map[string]string
			if len(fixture.Config) != 0 {
				data = make(map[string]string, len(fixture.Config))
			}
			var keys []string
			for key := range fixture.Config {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for index, key := range keys {
				name := "config-" + strconv.Itoa(index)
				data[name] = fixture.Config[key]
				options.fixtureConfigItems = append(options.fixtureConfigItems, corev1.KeyToPath{Key: name, Path: key})
			}
			config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: jobName(role) + "-config", Namespace: namespaceName(r, physicalSide)},
				Immutable: new(true), Data: data}
			if _, err := b.ensureObject(ctx, r, resourceKey(physicalSide, role+"-config"), config); err != nil {
				return nil, false, err
			}
			options.fixtureProfile, options.fixtureConfig = profile, config.Name
			input.FixtureSecrets, err = b.fixtureSecretFiles(ctx, r, side, fixture, profile)
			if err != nil {
				return nil, false, err
			}
			options.fixtureSecrets = input.FixtureSecrets
			options.hostAliases, err = b.fixtureAliases(ctx, r, side)
			if err != nil {
				return nil, false, err
			}
		}
		job, err := b.ensureJob(ctx, r, physicalSide, role, input, options)
		if err != nil {
			return nil, false, err
		}
		jobs = append(jobs, fixtureJob{fixture: fixture, role: role, imageID: imageID, job: job})
	}
	return b.observeFixtures(ctx, r, physicalSide, jobs)
}

type fixtureJob struct {
	fixture pv.ApplicationFixture
	role    string
	imageID string
	job     *batchv1.Job
}

func (b *Backend) observeFixtures(ctx context.Context, r *run, side string, jobs []fixtureJob) ([]pv.ApplicationEndpoint, bool, error) {
	endpoints := make([]pv.ApplicationEndpoint, 0, len(jobs))
	allReady := true
	for _, fixture := range jobs {
		pod, identity, ready, err := b.observeJob(ctx, r, side, fixture.role, fixture.job, fixture.imageID, false)
		if err != nil {
			return nil, false, err
		}
		if !ready {
			allReady = false
			continue
		}
		if net.ParseIP(pod.Status.PodIP) == nil {
			return nil, false, contract.ErrConflict
		}
		endpoints = append(endpoints, pv.ApplicationEndpoint{ID: fixture.fixture.ID, Address: pod.Status.PodIP, Port: fixture.fixture.Port, Pod: identity})
	}
	if !allReady {
		return nil, false, nil
	}
	return endpoints, true, nil
}

func (b *Backend) finalRecord(ctx context.Context, r *run) (*pv.Record, error) {
	if r.state.Binding == nil || r.state.Manifest == nil || !r.state.RecordCreated || !r.state.Finished || !r.state.Settled {
		return nil, contract.ErrConflict
	}
	record, err := b.store.GetPatchVerificationRun(ctx, r.state.Binding.RunID)
	if err != nil {
		return nil, safeError(contract.ErrUnavailable, err)
	}
	if record == nil || record.Binding != *r.state.Binding || !reflect.DeepEqual(record.Manifest, *r.state.Manifest) ||
		record.State != pv.RunFinalized || record.Seal == nil || record.Seal.Digest != pv.Digest(record.Seal.Content) ||
		!reflect.DeepEqual(record.Assessment, record.Seal.Assessment) || len(record.Incidents) != 0 {
		return nil, contract.ErrConflict
	}
	observations := make([]pv.Observation, 0, len(record.Evidence))
	for _, evidence := range record.Evidence {
		content, err := json.Marshal(evidence.Observation)
		if err != nil || evidence.Rejection != "" || evidence.Digest != pv.Digest(content) ||
			pv.ValidateObservation(record.Manifest, record.Binding, evidence.Observation) != nil {
			return nil, contract.ErrConflict
		}
		observations = append(observations, evidence.Observation)
	}
	if !reflect.DeepEqual(pv.Evaluate(record.Manifest, record.Binding, observations), record.Assessment) {
		return nil, contract.ErrConflict
	}
	return record, nil
}

func (b *Backend) cancelRecord(ctx context.Context, r *run) error {
	if r.state.Binding == nil || r.state.Finished {
		return nil
	}
	record, err := b.store.GetPatchVerificationRun(ctx, r.state.Binding.RunID)
	if errors.Is(err, pv.ErrRunNotFound) && !r.state.RecordCreated {
		return nil
	}
	if err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if record.Binding != *r.state.Binding || !reflect.DeepEqual(record.Manifest, *r.state.Manifest) {
		return contract.ErrConflict
	}
	if record.State == pv.RunRunning {
		_, err = b.store.CancelPatchVerificationRun(ctx, *r.state.Binding)
	}
	return safeErrorOrNil(err)
}
