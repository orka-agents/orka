package patchverification

import (
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"time"
)

func ValidateApplicationArtifact(manifest Manifest, binding Binding, side string, artifact ApplicationArtifact) error {
	if manifest.Application == nil || ValidateManifest(manifest) != nil ||
		ValidateApplicationSourceMetadata(manifest) != nil || ValidateRunBinding(manifest, binding) != nil {
		return ErrBinding
	}
	planDigest, err := ApplicationPlanDigest(*manifest.Application)
	if err != nil {
		return err
	}
	source, taskID, err := applicationSource(manifest, binding, side)
	if err != nil || artifact.Version != ApplicationProtocolVersion ||
		artifact.RunID != binding.RunID || artifact.AttemptID != binding.AttemptID ||
		artifact.ManifestDigest != binding.ManifestDigest || artifact.PlanDigest != planDigest ||
		artifact.Side != side || artifact.TaskID != taskID || artifact.Source != source ||
		artifact.GoVersion != manifest.Application.Build.GoVersion ||
		artifact.Platform != manifest.Environment.Platform || artifact.BinaryBytes <= 0 ||
		artifact.BinaryBytes > MaxApplicationBinaryBytes || !sha256Pattern.MatchString(artifact.BinaryDigest) ||
		artifact.Build.ImageID != manifest.Environment.ImageID || !applicationContainerValid(artifact.Build) {
		return errors.New("application artifact does not bind the exact source, procedure, toolchain and build container")
	}
	return nil
}

// BindApplicationBuildReport adds API-observed build identity to the trusted
// worker's measured artifact. The caller must read the exact completed build Pod,
// verify its admitted spec and image, and supply that identity independently.
func BindApplicationBuildReport(input ApplicationWorkerInput, report ApplicationBuildReport, identity ApplicationContainerIdentity) (ApplicationArtifact, error) {
	if ValidateApplicationWorkerInput(input) != nil || report.Version != ApplicationProtocolVersion ||
		report.TaskUID != input.TaskUID || report.PodUID != identity.PodUID ||
		!applicationContainerValid(identity) || identity.ImageID != input.Manifest.Environment.ImageID ||
		report.Artifact.Build.PodUID != identity.PodUID ||
		report.Artifact.Build.ImageID != identity.ImageID ||
		!applicationPartialContainerMatches(report.Artifact.Build, identity) {
		return ApplicationArtifact{}, ErrBinding
	}
	artifact := report.Artifact
	artifact.Build = identity
	if err := ValidateApplicationArtifact(input.Manifest, input.Binding, input.Side, artifact); err != nil {
		return ApplicationArtifact{}, err
	}
	return artifact, nil
}

func applicationPartialContainerMatches(partial, actual ApplicationContainerIdentity) bool {
	if partial.PodName != "" && partial.PodName != actual.PodName ||
		partial.PodUID != "" && partial.PodUID != actual.PodUID ||
		partial.ContainerName != "" && partial.ContainerName != actual.ContainerName ||
		partial.ContainerID != "" && partial.ContainerID != actual.ContainerID ||
		partial.ImageID != "" && partial.ImageID != actual.ImageID ||
		partial.JobUID != "" && partial.JobUID != actual.JobUID ||
		partial.Namespace != "" && partial.Namespace != actual.Namespace ||
		partial.NamespaceUID != "" && partial.NamespaceUID != actual.NamespaceUID ||
		partial.ServiceAccountName != "" && partial.ServiceAccountName != actual.ServiceAccountName ||
		partial.ServiceAccountUID != "" && partial.ServiceAccountUID != actual.ServiceAccountUID {
		return false
	}
	return true
}

// ValidateApplicationArtifactFile compares the build-produced PVC receipt with
// the API-enriched immutable expected artifact. The incomplete build identity
// on disk is not an authority and can never replace the backend's full identity.
func ValidateApplicationArtifactFile(expected, onDisk ApplicationArtifact) error {
	if !applicationPartialContainerMatches(onDisk.Build, expected.Build) ||
		onDisk.Build.PodUID != expected.Build.PodUID || onDisk.Build.ImageID != expected.Build.ImageID {
		return ErrBinding
	}
	onDisk.Build = expected.Build
	if !reflect.DeepEqual(expected, onDisk) {
		return ErrBinding
	}
	return nil
}

func validateApplicationObservation(manifest Manifest, binding Binding, check Check, observation Observation, pendingReplica bool) error {
	if err := validateApplicationObserverIdentity(manifest, check, observation); err != nil {
		return err
	}
	app := observation.Application
	if err := ValidateApplicationArtifact(manifest, binding, observation.Side, app.Artifact); err != nil {
		return err
	}
	if app.PlanDigest != app.Artifact.PlanDigest {
		return ErrBinding
	}
	if err := validateApplicationActionEvidence(manifest, check, app); err != nil {
		return err
	}
	if err := validateApplicationNormalAPIObservation(manifest, check, app); err != nil {
		return err
	}
	if err := ValidateApplicationSubjectTLSIdentities(manifest.ApplicationPolicy, app.Target, app.SubjectTLS); err != nil {
		return err
	}
	if err := validateApplicationReplicaEvidence(manifest, check, observation, pendingReplica); err != nil {
		return err
	}
	var result any
	switch {
	case check.Application.Kubernetes != nil:
		if err := validateApplicationAPIObservation(check.Application.Kubernetes, app); err != nil {
			return err
		}
		result = app.Kubernetes.Result
	case check.Application.TCP != nil:
		if err := validateApplicationTCPObservation(manifest, check.Application.TCP, observation); err != nil {
			return err
		}
		result = app.TCP.Result
	default:
		return ErrEvidence
	}
	content, _ := json.Marshal(result)
	if observation.StdoutDigest != Digest(content) || observation.StdoutBytes != len(content) ||
		observation.StderrDigest != Digest(nil) || observation.StderrBytes != 0 ||
		observation.ExitCode == nil || *observation.ExitCode != 0 {
		return errors.New("application observation bytes do not match the independent measured metadata")
	}
	return nil
}

func validateApplicationObserverIdentity(manifest Manifest, check Check, observation Observation) error {
	app := observation.Application
	if manifest.Application == nil || check.Application == nil || app == nil ||
		app.Version != ApplicationProtocolVersion || !app.Completed ||
		!applicationTargetValid(app.Target) || observation.Origin != ApplicationObserverOrigin ||
		observation.ImageID != manifest.Environment.Dependencies[ApplicationObserverImageIDKey] ||
		!identifierPattern.MatchString(observation.PodUID) || observation.ContainerID == "" ||
		observation.HTTPCompleted || len(observation.ServiceOutputs) != 0 ||
		!applicationContainerValid(app.Subject) ||
		app.Subject.ImageID != manifest.Application.Runtime.ImageID ||
		app.Subject.PodUID == observation.PodUID || app.Subject.ContainerID == observation.ContainerID ||
		app.Artifact.Build.PodUID == observation.PodUID || app.Artifact.Build.PodUID == app.Subject.PodUID ||
		app.Artifact.Build.ContainerID == observation.ContainerID || app.Artifact.Build.ContainerID == app.Subject.ContainerID {
		return errors.New("application evidence requires a completed independent observer and exact subject identity")
	}
	return nil
}

func validateApplicationAPIObservation(expected *ApplicationKubernetesCheck, app *ApplicationObservation) error {
	actual := app.Kubernetes
	if actual == nil || app.TCP != nil || actual.APIVersion != expected.APIVersion ||
		actual.Resource != expected.Resource || actual.Name != expected.Name ||
		actual.Pointer != "" && actual.Pointer != expected.Pointer || !applicationAPIResultValid(actual.Result) {
		return ErrBinding
	}
	if actual.Result.Exists {
		if !identifierPattern.MatchString(actual.UID) || actual.ResourceVersion == "" || len(actual.ResourceVersion) > 128 {
			return ErrBinding
		}
	} else if actual.UID != "" || actual.ResourceVersion != "" {
		return ErrBinding
	}
	return nil
}

func validateApplicationTCPObservation(manifest Manifest, expected *ApplicationTCPCheck, observation Observation) error {
	app, actual := observation.Application, observation.Application.TCP
	if actual == nil || app.Kubernetes != nil || actual.Endpoint.ID != expected.Target ||
		actual.Endpoint.Port != expected.Port || !applicationContainerValid(actual.Endpoint.Pod) ||
		net.ParseIP(actual.Endpoint.Address) == nil ||
		actual.MarkerID != expected.MarkerID || !sha256Pattern.MatchString(actual.MarkerDigest) ||
		actual.MarkerBytes < 32 || actual.MarkerBytes > 128 ||
		actual.Bytes < 0 || actual.Bytes > expected.MaxBytes {
		return ErrBinding
	}
	if err := validateApplicationTCPCompletion(expected, actual, observation); err != nil {
		return err
	}
	if actual.Result.MarkerMatched && actual.Bytes < actual.MarkerBytes {
		return ErrEvidence
	}
	if manifest.ApplicationPolicy != nil {
		if err := validateApplicationMarkerReference(actual, app.Target); err != nil {
			return err
		}
	}
	if expected.Target == ApplicationSubject && actual.Endpoint.Pod != app.Subject {
		return errors.New("TCP evidence is not from the measured application container")
	}
	if expected.Target != ApplicationSubject && actual.Endpoint.Pod.ImageID != manifest.Environment.Dependencies[ApplicationObserverImageIDKey] {
		return ErrBinding
	}
	return nil
}

func validateApplicationTCPCompletion(expected *ApplicationTCPCheck, actual *ApplicationTCPIdentity, observation Observation) error {
	if expected.Mode == ApplicationTCPReceive {
		if actual.Mode != ApplicationTCPReceive || actual.WindowSeconds != expected.WindowSeconds ||
			actual.Connections < 0 || actual.Connections > 8 ||
			actual.Result.Connected != (actual.Connections > 0) ||
			observation.FinishedAt.Sub(observation.StartedAt) < time.Duration(expected.WindowSeconds)*time.Second ||
			actual.Result.MarkerMatched && !actual.Result.Connected {
			return ErrEvidence
		}
		if actual.Endpoint.Pod.PodUID != observation.PodUID || actual.Endpoint.Pod.ContainerID != observation.ContainerID {
			return ErrBinding
		}
		if actual.ReceiverRoute == nil || !applicationReceiverRouteIdentityValid(*actual.ReceiverRoute, actual.Endpoint.Pod) {
			return ErrBinding
		}
	} else if !actual.Result.Connected || actual.Mode != "" || actual.WindowSeconds != 0 ||
		actual.Connections != 0 || actual.ReceiverRoute != nil {
		return ErrEvidence
	}
	return nil
}

func validateApplicationMarkerReference(actual *ApplicationTCPIdentity, target ApplicationTarget) error {
	ref := actual.MarkerRef
	if ref == nil || ref.Digest != actual.MarkerDigest || !applicationName(ref.Namespace) ||
		!identifierPattern.MatchString(ref.NamespaceUID) || ref.Namespace == target.Namespace ||
		ref.NamespaceUID == target.NamespaceUID || !applicationName(ref.Name) ||
		!identifierPattern.MatchString(ref.UID) || ref.Key == "" {
		return ErrBinding
	}
	return nil
}

// MatchedApplicationOutput exposes only already-frozen observation metadata.
// Neither subject logs, raw response bytes nor protected marker values enter it.
func MatchedApplicationOutput(check Check, observation Observation) (string, bool) {
	if check.ID != observation.CheckID || check.Application == nil ||
		!usableCheck(check, observation) || observation.Origin != ApplicationObserverOrigin {
		return "", false
	}
	var result any
	app := observation.Application
	if check.Application.Kubernetes != nil && app.Kubernetes != nil && app.TCP == nil {
		result = app.Kubernetes.Result
	} else if check.Application.TCP != nil && app.TCP != nil && app.Kubernetes == nil {
		result = app.TCP.Result
	} else {
		return "", false
	}
	content, _ := json.Marshal(result)
	if observation.StdoutDigest != Digest(content) || observation.StdoutBytes != len(content) {
		return "", false
	}
	for _, expected := range []Expectation{check.Healthy, check.Failure} {
		if matches(observation, expected) {
			return expected.Stdout, true
		}
	}
	return "", false
}

func validateApplicationObservationSet(manifest Manifest, observations []Observation) error {
	if manifest.Application == nil {
		return nil
	}
	artifacts := map[string]ApplicationArtifact{}
	targetCluster := ""
	for _, observation := range observations {
		if observation.Application == nil {
			return ErrEvidence
		}
		actual := observation.Application
		if previous, found := artifacts[observation.Side]; found && previous != actual.Artifact {
			return errors.New("application checks for one side must use the same measured compiled artifact")
		}
		artifacts[observation.Side] = actual.Artifact
		if targetCluster != "" && targetCluster != actual.Target.ClusterUID {
			return errors.New("original and candidate application checks must use the same isolated target cluster")
		}
		targetCluster = actual.Target.ClusterUID
	}
	original, hasOriginal := artifacts[Original]
	patched, hasPatched := artifacts[Patched]
	if manifest.Action == VerifyPatch && hasOriginal && hasPatched && original.BinaryDigest == patched.BinaryDigest {
		return errors.New("identical compiled artifacts cannot establish a changed application-runtime behavior")
	}
	return nil
}

func ValidateApplicationBoundaryReport(input ApplicationBoundaryInput, report ApplicationBoundaryReport) error {
	fileSize, err := ApplicationFileSizeLimit(input.FileSizeRole)
	if err != nil || report.FileSizeSoft != fileSize || report.FileSizeHard != fileSize {
		return errors.New("application worker lacks its exact inherited non-raiseable file-size limit")
	}
	if err := validateApplicationRuntimeBoundary(input, report); err != nil {
		return err
	}
	digest, err := ApplicationBoundaryInputDigest(input)
	if err != nil || report.Version != ApplicationProtocolVersion || report.InputDigest != digest ||
		report.TaskUID != input.TaskUID || !identifierPattern.MatchString(report.PodUID) ||
		!applicationBoundaryProcessValid(report) || report.CanaryReachable != input.PositiveControl {
		return errors.New("application boundary measurements do not meet the fixed runtime limits")
	}
	if err := validateApplicationSourceBoundary(input, report); err != nil {
		return err
	}
	if err := validateApplicationBoundaryCapabilities(report.Capabilities); err != nil {
		return err
	}
	return validateApplicationBoundaryAuthorization(input, report)
}

func applicationBoundaryProcessValid(report ApplicationBoundaryReport) bool {
	return report.UID == 65532 && report.GID == 65532 && report.PID == 1 &&
		report.SeccompMode == 2 && report.NoNewPrivileges && report.ReadOnlyRoot &&
		report.MemoryMax > 0 && report.MemoryMax <= 2<<30 &&
		report.PIDsMax > 0 && report.PIDsMax <= 512 && report.SwapMax == 0 &&
		report.CPUQuota > 0 && report.CPUPeriod > 0 && report.CPUPeriod <= 1000000 &&
		report.CPUQuota <= 2*report.CPUPeriod
}

func validateApplicationSourceBoundary(input ApplicationBoundaryInput, report ApplicationBoundaryReport) error {
	if input.PositiveControl && input.SourceHost != "" {
		if !report.SourceHTTPSVerified || report.SourceHTTPStatus < 200 || report.SourceHTTPStatus > 399 {
			return errors.New("application source relay did not establish authenticated public HTTPS reachability")
		}
	} else if report.SourceHTTPSVerified || report.SourceHTTPStatus != 0 {
		return ErrEvidence
	}
	return nil
}

func validateApplicationBoundaryCapabilities(capabilities map[string]string) error {
	if len(capabilities) != 5 {
		return ErrEvidence
	}
	for _, name := range []string{"CapEff", "CapPrm", "CapBnd", "CapInh", "CapAmb"} {
		if capabilities[name] != "0000000000000000" {
			return ErrEvidence
		}
	}
	return nil
}

func validateApplicationBoundaryAuthorization(input ApplicationBoundaryInput, report ApplicationBoundaryReport) error {
	if input.APIAddress == "" {
		if len(report.APIStatuses) != 0 || report.APIUserUID != "" {
			return ErrEvidence
		}
	} else {
		count := 5
		if input.PositiveControl {
			count++
		}
		if len(report.APIStatuses) != count || !identifierPattern.MatchString(report.APIUserUID) ||
			report.APIStatuses["self-identity"] != 201 ||
			input.PositiveControl && report.APIStatuses["self-pod"] != 200 {
			return ErrEvidence
		}
		for _, name := range []string{"foreign-namespace", applicationSecrets, "token-request", "cluster-list"} {
			if report.APIStatuses[name] != 403 {
				return errors.New("application test identity has forbidden authority or the target API was not reachable")
			}
		}
	}
	return nil
}
