package patchverification

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func applicationTestManifest(t *testing.T) Manifest {
	t.Helper()
	manifest := testManifest()
	manifest.Action = VerifyPatch
	manifest.ReportDigest, _ = StableReportDigest(manifest.Problem, manifest.Scope)
	manifest.DeclaredChanges = []DeclaredChange{{Kind: "source", Description: "repair the application behavior"}}
	manifest.Sources.Repository = "https://github.com/example/public-source"
	manifest.Environment.Profile = ApplicationRuntimeProfile
	manifest.Environment.Dependencies = map[string]string{
		ApplicationPolicyKey:          ApplicationPolicyVersion,
		ApplicationObserverImageKey:   "example/observer@" + Digest([]byte("observer-image")),
		ApplicationObserverImageIDKey: Digest([]byte("observer-config")),
	}
	manifest.Application = &ApplicationPlan{
		Version: ApplicationProtocolVersion,
		Build:   ApplicationBuild{Profile: ApplicationBuildGoPackage, Package: "./cmd/server", GoVersion: "go1.27.2"},
		Runtime: ApplicationRuntime{Image: "example/runtime@" + Digest([]byte("runtime-image")),
			ImageID: Digest([]byte("runtime-config")), Ports: []int{8080}},
		Permissions: []ApplicationPermission{{Resources: []string{"services/status"}, Verbs: []string{"patch"}}},
	}
	manifest.ApplicationSourceMetadata = &ApplicationSourceMetadata{
		Version:  ApplicationProtocolVersion,
		Original: ApplicationArchiveMetadata{ArchiveDigest: manifest.Sources.Original.ArchiveDigest, ArchiveBytes: 4096, ContentBytes: 100, Entries: 2},
		Patched:  &ApplicationArchiveMetadata{ArchiveDigest: manifest.Sources.Patched.ArchiveDigest, ArchiveBytes: 4096, ContentBytes: 100, Entries: 2},
	}
	healthy, err := ApplicationAPIExpectation(ApplicationAPIValue{Exists: true, Found: true, Value: []byte(`false`)})
	require.NoError(t, err)
	failure, err := ApplicationAPIExpectation(ApplicationAPIValue{Exists: true, Found: true, Value: []byte(`true`)})
	require.NoError(t, err)
	manifest.Checks = []Check{
		{ID: "unsafe", Kind: Reproduction, TimeoutSeconds: 10, Healthy: healthy, Failure: failure,
			Application: &ApplicationCheck{Version: ApplicationProtocolVersion, Kubernetes: &ApplicationKubernetesCheck{
				APIVersion: "v1", Resource: "services", Name: "case-unsafe", Pointer: "/status/accepted"}}},
		{ID: "normal", Kind: Normal, TimeoutSeconds: 10, Healthy: failure, Failure: healthy,
			Application: &ApplicationCheck{Version: ApplicationProtocolVersion,
				Resources: []json.RawMessage{[]byte(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"case-normal"},"spec":{"type":"ClusterIP","selector":{"orka.ai/application-target":"subject"},"ports":[{"port":8080,"targetPort":8080,"protocol":"TCP"}]}}`)},
				Kubernetes: &ApplicationKubernetesCheck{
					APIVersion: "v1", Resource: "services", Name: "case-normal", Pointer: "/status/accepted", NormalValue: []byte("true")}}},
	}
	require.NoError(t, ValidateManifest(manifest))
	return manifest
}

func applicationTestIdentity(name, image string) ApplicationContainerIdentity {
	return ApplicationContainerIdentity{
		PodName: name, PodUID: name + "-uid", ContainerName: "worker", ContainerID: "containerd://" + name,
		ImageID: image, JobUID: name + "-job-uid", ServiceAccountName: name + "-sa", ServiceAccountUID: name + "-sa-uid",
	}
}

func applicationTestEvidence(t *testing.T, manifest Manifest) (Binding, []Observation) {
	t.Helper()
	patchedTask := "patched-task"
	if manifest.Action == ValidateReport {
		patchedTask = ""
	}
	binding, err := NewRunBinding(manifest, "application-attempt", "original-task", patchedTask)
	require.NoError(t, err)
	planDigest, err := ApplicationPlanDigest(*manifest.Application)
	require.NoError(t, err)
	observations := make([]Observation, 0, len(ActionSides(manifest.Action))*len(manifest.Checks))
	for _, side := range ActionSides(manifest.Action) {
		source, taskID, err := applicationSource(manifest, binding, side)
		require.NoError(t, err)
		artifact := ApplicationArtifact{
			Version: ApplicationProtocolVersion, RunID: binding.RunID, AttemptID: binding.AttemptID,
			TaskID: taskID, ManifestDigest: binding.ManifestDigest, PlanDigest: planDigest,
			Side: side, Source: source, BinaryDigest: Digest([]byte("binary-" + side)), BinaryBytes: 123,
			GoVersion: manifest.Application.Build.GoVersion, Platform: manifest.Environment.Platform,
			Build: applicationTestIdentity("build-"+side, manifest.Environment.ImageID),
		}
		for _, check := range manifest.Checks {
			expected := check.Healthy
			if side == Original && check.Kind == Reproduction {
				expected = check.Failure
			}
			worker := applicationTestIdentity("observer-"+side+"-"+check.ID, manifest.Environment.Dependencies[ApplicationObserverImageIDKey])
			subject := applicationTestIdentity("subject-"+side+"-"+check.ID, manifest.Application.Runtime.ImageID)
			app := &ApplicationObservation{Version: ApplicationProtocolVersion, Completed: true,
				PlanDigest: planDigest, Target: ApplicationTarget{ClusterUID: "target-cluster", Namespace: "run-" + side, NamespaceUID: "namespace-" + side},
				Artifact: artifact, Subject: subject}
			if api := check.Application.Kubernetes; api != nil {
				var result ApplicationAPIResult
				require.NoError(t, json.Unmarshal([]byte(expected.Stdout), &result))
				app.Kubernetes = &ApplicationAPIIdentity{APIVersion: api.APIVersion, Resource: api.Resource, Name: api.Name, Pointer: api.Pointer,
					UID: "resource-" + side + "-" + check.ID, ResourceVersion: "123", Result: result}
				if ApplicationUsesAPINormalTransition(check, manifest.ApplicationPolicy) {
					app.APIBaseline = &ApplicationAPIIdentity{APIVersion: api.APIVersion, Resource: api.Resource, Name: api.Name,
						Pointer: api.Pointer, UID: app.Kubernetes.UID, ResourceVersion: "122", Result: ApplicationAPIResult{Exists: true}}
				}
			} else {
				var result ApplicationTCPValue
				require.NoError(t, json.Unmarshal([]byte(expected.Stdout), &result))
				app.TCP = &ApplicationTCPIdentity{
					Endpoint: ApplicationEndpoint{ID: "subject", Address: "10.10.0.4", Port: check.Application.TCP.Port, Pod: subject},
					MarkerID: check.Application.TCP.MarkerID, MarkerDigest: Digest([]byte("protected-marker-" + side)),
					MarkerBytes: 48, Bytes: 64, Result: result,
				}
			}
			started := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
			observations = append(observations, Observation{
				RunID: binding.RunID, AttemptID: binding.AttemptID, TaskID: taskID, ManifestDigest: binding.ManifestDigest,
				Side: side, CheckID: check.ID, SourceTree: source.Tree, ImageID: worker.ImageID,
				ContainerID: worker.ContainerID, PodUID: worker.PodUID, JobUID: worker.JobUID,
				Origin: "application-observer", StartedAt: started, FinishedAt: started.Add(time.Second),
				Executed: true, ExitCode: new(0), StdoutDigest: Digest([]byte(expected.Stdout)),
				StdoutBytes: len(expected.Stdout), StderrDigest: Digest(nil), Application: app,
			})
		}
	}
	return binding, observations
}

func TestApplicationManifestRejectsUnsafeOrUnsupportedExecution(t *testing.T) {
	tests := map[string]func(*Manifest){
		"shell build profile":        func(m *Manifest) { m.Application.Build.Profile = "shell" },
		"build flag injection":       func(m *Manifest) { m.Application.Build.Package = "-toolexec=/bin/sh" },
		"package traversal":          func(m *Manifest) { m.Application.Build.Package = "./cmd/../outside" },
		"recursive package wildcard": func(m *Manifest) { m.Application.Build.Package = "./..." },
		"absolute package":           func(m *Manifest) { m.Application.Build.Package = "/etc" },
		"unpinned compiler":          func(m *Manifest) { m.Application.Build.GoVersion = "latest" },
		"unpinned runtime":           func(m *Manifest) { m.Application.Runtime.Image = "example/runtime:latest" },
		"privileged port":            func(m *Manifest) { m.Application.Runtime.Ports = []int{80} },
		"scoped source":              func(m *Manifest) { m.Sources.Paths = []string{"cmd/server"} },
		"shell check":                func(m *Manifest) { m.Checks[0].Command = []string{"/bin/sh", "-c", "echo pass"} },
		"source harness":             func(m *Manifest) { m.Files = []FrozenFile{{Path: "main.go", Content: []byte("package main")}} },
		"environment injection":      func(m *Manifest) { m.Environment.Variables = map[string]string{"GOFLAGS": "-toolexec=bad"} },
		"fixture assertion":          func(m *Manifest) { m.Checks[0].Application.Kubernetes.Pointer = "/spec/result" },
		"secret observation":         func(m *Manifest) { m.Checks[0].Application.Kubernetes.Resource = "secrets" },
		"pod exec":                   func(m *Manifest) { m.Checks[0].Application.Kubernetes.Resource = "pods/exec" },
		"credential authority": func(m *Manifest) {
			m.Application.Permissions = []ApplicationPermission{{Resources: []string{"secrets"}, Verbs: []string{"get"}}}
		},
		"wildcard authority": func(m *Manifest) {
			m.Application.Permissions = []ApplicationPermission{{Resources: []string{"*"}, Verbs: []string{"get"}}}
		},
		"create authority": func(m *Manifest) {
			m.Application.Permissions = []ApplicationPermission{{Resources: []string{"pods"}, Verbs: []string{"create"}}}
		},
		"missing normal":         func(m *Manifest) { m.Checks = m.Checks[:1] },
		"unmatched normal":       func(m *Manifest) { m.Checks[1].Application.Kubernetes.Pointer = "/status/different" },
		"wrong worker policy":    func(m *Manifest) { m.Environment.Dependencies["orka.kubernetes.policy"] = KubernetesPolicyVersion },
		"missing observer image": func(m *Manifest) { delete(m.Environment.Dependencies, ApplicationObserverImageIDKey) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			manifest := applicationTestManifest(t)
			mutate(&manifest)
			require.Error(t, ValidateManifest(manifest))
		})
	}
}

func TestApplicationResourceAdmissionRejectsPrivilegeAndAuthority(t *testing.T) {
	for _, raw := range []string{
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"candidate"},"spec":{"hostNetwork":true}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"input","namespace":"kube-system"},"data":{}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"input","ownerReferences":[{"uid":"other"}]}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"orka-input"}}`,
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credential"},"data":{}}`,
		`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"authority"},"rules":[]}`,
		`{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"custom.example"}}`,
		`{"apiVersion":"v1","kind":"Service","metadata":{"name":"fixture"},"spec":{"type":"ExternalName","externalName":"example.com"}}`,
		`{"apiVersion":"v1","kind":"Service","metadata":{"name":"fixture"},"spec":{"type":"NodePort","ports":[{"port":8080,"targetPort":8080,"nodePort":30000}],"selector":{"orka.ai/application-target":"subject"}}}`,
		`{"apiVersion":"v1","kind":"Service","metadata":{"name":"fixture"},"spec":{"ports":[{"port":8080,"targetPort":8080}],"selector":{"app":"control"}}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"input"},"data":{},"status":{"passed":true}}`,
	} {
		_, err := ValidateApplicationResource([]byte(raw))
		require.Error(t, err)
	}
	for _, raw := range []string{
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"fixture"},"data":{"setting":"untrusted data"}}`,
		`{"apiVersion":"v1","kind":"Service","metadata":{"name":"fixture"},"spec":{"type":"ClusterIP","ports":[{"port":8080,"targetPort":8080}],"selector":{"orka.ai/application-target":"subject"}}}`,
		`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"fixture"},"spec":{"defaultBackend":{"service":{"name":"fixture","port":{"number":8080}}}}}`,
	} {
		_, err := ValidateApplicationResource([]byte(raw))
		require.NoError(t, err)
	}
}

func TestApplicationEvidenceRequiresExactArtifactAndIndependentCompletion(t *testing.T) {
	manifest := applicationTestManifest(t)
	binding, evidence := applicationTestEvidence(t, manifest)
	require.Equal(t, Verified, Evaluate(manifest, binding, evidence).Conclusion)
	for _, observation := range evidence {
		require.NoError(t, ValidateObservation(manifest, binding, observation))
	}
	tests := map[string]func(*Observation){
		"source harness only":         func(o *Observation) { o.Application = nil; o.HTTPCompleted = true },
		"subject stdout":              func(o *Observation) { o.Origin = "runner" },
		"incomplete observer":         func(o *Observation) { o.Application.Completed = false },
		"wrong archive":               func(o *Observation) { o.Application.Artifact.Source.ArchiveDigest = Digest([]byte("other")) },
		"wrong tree":                  func(o *Observation) { o.Application.Artifact.Source.Tree = strings.Repeat("e", 40) },
		"wrong source side":           func(o *Observation) { o.Application.Artifact.Side = Original },
		"wrong procedure":             func(o *Observation) { o.Application.Artifact.PlanDigest = Digest([]byte("different plan")) },
		"wrong compiler":              func(o *Observation) { o.Application.Artifact.GoVersion = "go1.1.1" },
		"wrong builder":               func(o *Observation) { o.Application.Artifact.Build.ImageID = Digest([]byte("wrong image")) },
		"no binary digest":            func(o *Observation) { o.Application.Artifact.BinaryDigest = "" },
		"no build identity":           func(o *Observation) { o.Application.Artifact.Build.ContainerID = "" },
		"wrong subject image":         func(o *Observation) { o.Application.Subject.ImageID = Digest([]byte("other runtime")) },
		"subject self-observation":    func(o *Observation) { o.Application.Subject.PodUID = o.PodUID },
		"wrong observer image":        func(o *Observation) { o.ImageID = manifest.Environment.ImageID },
		"wrong namespace incarnation": func(o *Observation) { o.Application.Target.NamespaceUID = "" },
		"wrong API identity":          func(o *Observation) { o.Application.Kubernetes.Name = "different" },
		"forged API stdout":           func(o *Observation) { o.Application.Kubernetes.Result.ValueDigest = Digest([]byte("unrelated")) },
		"truncated":                   func(o *Observation) { o.OutputTruncated = true },
		"timeout":                     func(o *Observation) { o.TimedOut = true },
		"subject exit zero only":      func(o *Observation) { o.Executed = false },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			binding, evidence := applicationTestEvidence(t, manifest)
			mutate(&evidence[2])
			require.Equal(t, UnableToVerify, Evaluate(manifest, binding, evidence).Conclusion)
		})
	}
}

func TestApplicationComparisonsRequireMatchedNormalControlsAndSameArtifact(t *testing.T) {
	manifest := applicationTestManifest(t)
	binding, evidence := applicationTestEvidence(t, manifest)
	require.Equal(t, UnableToVerify, Evaluate(manifest, binding, evidence[:3]).Conclusion)
	evidence[3].Application.Artifact.BinaryDigest = Digest([]byte("another binary for the normal case"))
	require.Equal(t, UnableToVerify, Evaluate(manifest, binding, evidence).Conclusion)
	binding, evidence = applicationTestEvidence(t, manifest)
	evidence[2].Application.Artifact.BinaryDigest = evidence[0].Application.Artifact.BinaryDigest
	evidence[3].Application.Artifact.BinaryDigest = evidence[0].Application.Artifact.BinaryDigest
	require.Equal(t, UnableToVerify, Evaluate(manifest, binding, evidence).Conclusion,
		"environment drift cannot establish a fix when the only runtime artifact is identical")
	binding, evidence = applicationTestEvidence(t, manifest)
	evidence[2].Application.Target.ClusterUID = "another-cluster"
	require.Equal(t, UnableToVerify, Evaluate(manifest, binding, evidence).Conclusion)
	binding, evidence = applicationTestEvidence(t, manifest)
	var result ApplicationAPIResult
	require.NoError(t, json.Unmarshal([]byte(manifest.Checks[1].Failure.Stdout), &result))
	evidence[3].Application.Kubernetes.Result = result
	evidence[3].StdoutDigest = Digest([]byte(manifest.Checks[1].Failure.Stdout))
	evidence[3].StdoutBytes = len(manifest.Checks[1].Failure.Stdout)
	require.Equal(t, Regression, Evaluate(manifest, binding, evidence).Conclusion)
}

func TestApplicationReportRequiresNormalControlBeforeReproduced(t *testing.T) {
	manifest := applicationTestManifest(t)
	manifest.Action, manifest.DeclaredChanges = ValidateReport, nil
	manifest.Sources.Patched, manifest.Sources.DiffDigest = SourceIdentity{}, ""
	manifest.ApplicationSourceMetadata.Patched = nil
	binding, evidence := applicationTestEvidence(t, manifest)
	require.Equal(t, Reproduced, Evaluate(manifest, binding, evidence).Conclusion)
	require.Equal(t, UnableToValidate, Evaluate(manifest, binding, evidence[:1]).Conclusion)
	manifest.Checks = manifest.Checks[:1]
	require.Error(t, ValidateManifest(manifest))
}

func TestApplicationCanonicalMetadataNeverContainsObservedValues(t *testing.T) {
	secret := "synthetic-untrusted-observed-value-not-a-credential"
	expected, err := ApplicationAPIExpectation(ApplicationAPIValue{Exists: true, Found: true, Value: []byte(`"` + secret + `"`)})
	require.NoError(t, err)
	require.NotContains(t, expected.Stdout, secret)
	require.Contains(t, expected.Stdout, Digest([]byte(`"`+secret+`"`)))
	manifest := applicationTestManifest(t)
	_, evidence := applicationTestEvidence(t, manifest)
	output, ok := MatchedApplicationOutput(manifest.Checks[0], evidence[0])
	require.True(t, ok)
	require.Equal(t, manifest.Checks[0].Failure.Stdout, output)
	evidence[0].Application.Kubernetes.Result.ValueDigest = Digest([]byte("forged"))
	_, ok = MatchedApplicationOutput(manifest.Checks[0], evidence[0])
	require.False(t, ok)
}

func TestApplicationFieldsOmittedPreserveSourceHarnessCanonicalJSON(t *testing.T) {
	manifest := testManifest()
	content, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NotContains(t, string(content), `"application"`)
	binding, observations := testEvidence(manifest)
	content, err = json.Marshal(observations[0])
	require.NoError(t, err)
	require.NotContains(t, string(content), `"application"`)
	require.Equal(t, Verified, Evaluate(manifest, binding, observations).Conclusion)
	observations[0].Application = &ApplicationObservation{Completed: true}
	require.Error(t, ValidateObservation(manifest, binding, observations[0]))
}

func TestApplicationPreparationSupportsOnlyCompleteSource(t *testing.T) {
	manifest := applicationTestManifest(t)
	manifest.Action, manifest.DeclaredChanges = ValidateReport, nil
	manifest.Sources.Patched, manifest.Sources.DiffDigest, manifest.Sources.Original.ArchiveDigest = SourceIdentity{}, "", ""
	manifest.ApplicationSourceMetadata = nil
	manifest.Environment.Dependencies[PreparationPolicyKey] = PreparationPolicy
	manifest.Environment.Dependencies[PreparationConfigurationKey] = Digest([]byte("operator-configuration"))
	input := ApplicationPreparationInput{Version: PreparationProtocolVersion, ID: "prepare", Namespace: "control",
		ParentTaskName: "parent", ParentTaskUID: "parent-uid", TaskUID: "prepare-uid", Manifest: manifest}
	require.NoError(t, ValidatePreparationInput(input))
	input.Manifest.Sources.Paths = []string{"cmd/server"}
	require.Error(t, ValidatePreparationInput(input))
}
