package patchverification

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testManifest() Manifest {
	return Manifest{
		Version: SchemaVersion, Problem: "unsafe input is accepted", Scope: []string{"two unsafe inputs and normal input"},
		Sources:     Sources{Repository: "/local/project", Original: SourceIdentity{Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), ArchiveDigest: Digest([]byte("original"))}, Patched: SourceIdentity{Commit: strings.Repeat("c", 40), Tree: strings.Repeat("d", 40), ArchiveDigest: Digest([]byte("patched"))}, DiffDigest: Digest([]byte("diff"))},
		Environment: Environment{Image: "example/tool@" + Digest([]byte("image")), ImageID: Digest([]byte("image-config")), Platform: "linux/amd64", Profile: Offline},
		Checks: []Check{
			{ID: "case-one", Kind: Reproduction, Command: []string{"/checks/run", "one"}, Healthy: Expectation{Stdout: "rejected\n"}, Failure: Expectation{Stdout: "accepted\n"}, TimeoutSeconds: 10},
			{ID: "case-two", Kind: Reproduction, Command: []string{"/checks/run", "two"}, Healthy: Expectation{Stdout: "rejected\n"}, Failure: Expectation{Stdout: "accepted\n"}, TimeoutSeconds: 10},
			{ID: "normal", Kind: Normal, Command: []string{"/checks/run", "normal"}, Healthy: Expectation{Stdout: "accepted\n"}, Failure: Expectation{Stdout: "rejected\n"}, TimeoutSeconds: 10},
		},
	}
}

func testEvidence(manifest Manifest) (Binding, []Observation) {
	digest, _ := ManifestDigest(manifest)
	binding := Binding{RunID: "run-one", AttemptID: "attempt-one", OriginalTaskID: "task-original-uid", PatchedTaskID: "task-patched-uid", ManifestDigest: digest}
	observations := make([]Observation, 0, 2*len(manifest.Checks))
	for _, side := range []string{Original, Patched} {
		for _, check := range manifest.Checks {
			expected := check.Healthy
			task, tree := binding.PatchedTaskID, manifest.Sources.Patched.Tree
			if side == Original {
				task, tree = binding.OriginalTaskID, manifest.Sources.Original.Tree
				if check.Kind == Reproduction {
					expected = check.Failure
				}
			}
			started := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
			observations = append(observations, Observation{RunID: binding.RunID, AttemptID: binding.AttemptID, TaskID: task, ManifestDigest: digest, Side: side, CheckID: check.ID, SourceTree: tree, ImageID: manifest.Environment.ImageID, ContainerID: "container-" + side + "-" + check.ID, Origin: "runner", StartedAt: started, FinishedAt: started.Add(time.Second), Executed: true, ExitCode: new(expected.ExitCode), StdoutDigest: Digest([]byte(expected.Stdout)), StdoutBytes: len(expected.Stdout), StderrDigest: Digest(nil)})
		}
	}
	return binding, observations
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]Observation) []Observation
		want   Conclusion
	}{
		{name: "real fix", want: Verified},
		{name: "captured stderr", mutate: func(observations []Observation) []Observation {
			stderr := []byte("diagnostic\n")
			observations[3].StderrDigest, observations[3].StderrBytes = Digest(stderr), len(stderr)
			return observations
		}, want: Verified},
		{name: "not fixed", mutate: func(observations []Observation) []Observation {
			for index := 3; index < 5; index++ {
				observations[index].StdoutDigest, observations[index].StdoutBytes = observations[index-3].StdoutDigest, observations[index-3].StdoutBytes
			}
			return observations
		}, want: NotFixed},
		{name: "partial fix", mutate: func(observations []Observation) []Observation {
			observations[3].StdoutDigest, observations[3].StdoutBytes = observations[0].StdoutDigest, observations[0].StdoutBytes
			return observations
		}, want: PartiallyFixed},
		{name: "regression", mutate: func(observations []Observation) []Observation {
			observations[5].StdoutDigest, observations[5].StdoutBytes = observations[3].StdoutDigest, observations[3].StdoutBytes
			return observations
		}, want: Regression},
		{name: "zero checks", mutate: func([]Observation) []Observation { return nil }, want: UnableToVerify},
		{name: "missing patched check", mutate: func(observations []Observation) []Observation { return observations[:5] }, want: UnableToVerify},
		{name: "no reproduction", mutate: func(observations []Observation) []Observation {
			observations[0].StdoutDigest, observations[0].StdoutBytes = observations[3].StdoutDigest, observations[3].StdoutBytes
			return observations
		}, want: UnableToVerify},
		{name: "identical redelivery", mutate: func(observations []Observation) []Observation { return append(observations, observations[0]) }, want: Verified},
		{name: "conflicting redelivery", mutate: func(observations []Observation) []Observation {
			conflict := observations[0]
			conflict.StdoutDigest = Digest([]byte("different"))
			return append(observations, conflict)
		}, want: UnableToVerify},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := testManifest()
			binding, observations := testEvidence(manifest)
			if test.mutate != nil {
				observations = test.mutate(observations)
			}
			if got := Evaluate(manifest, binding, observations); got.Conclusion != test.want {
				t.Fatalf("got %s (%s), want %s", got.Conclusion, got.Reason, test.want)
			}
		})
	}
}

func TestEvaluateFailsClosed(t *testing.T) {
	tests := map[string]func(*Observation){
		"wrong run":             func(observation *Observation) { observation.RunID = "foreign" },
		"wrong task":            func(observation *Observation) { observation.TaskID = "foreign" },
		"wrong attempt":         func(observation *Observation) { observation.AttemptID = "foreign" },
		"changed manifest":      func(observation *Observation) { observation.ManifestDigest = Digest([]byte("new")) },
		"wrong tree":            func(observation *Observation) { observation.SourceTree = strings.Repeat("e", 40) },
		"wrong image":           func(observation *Observation) { observation.ImageID = Digest([]byte("other")) },
		"untrusted report":      func(observation *Observation) { observation.Origin = "project" },
		"zero executed":         func(observation *Observation) { observation.Executed = false },
		"skipped":               func(observation *Observation) { observation.Skipped = true },
		"timeout":               func(observation *Observation) { observation.TimedOut = true },
		"setup error":           func(observation *Observation) { observation.SetupError = "fixture unavailable" },
		"network blocked":       func(observation *Observation) { observation.SetupError = "required local HTTP service is unreachable" },
		"truncated evidence":    func(observation *Observation) { observation.OutputTruncated = true },
		"missing stderr digest": func(observation *Observation) { observation.StderrDigest = "" },
		"invalid stderr digest": func(observation *Observation) { observation.StderrDigest = "unknown" },
		"negative stderr bytes": func(observation *Observation) { observation.StderrBytes = -1 },
		"missing executable":    func(observation *Observation) { observation.ExitCode = new(127) },
		"crashed":               func(observation *Observation) { observation.ExitCode = new(139) },
		"unknown failure":       func(observation *Observation) { observation.StdoutDigest = Digest([]byte("unrelated failure")) },
		"missing exit code":     func(observation *Observation) { observation.ExitCode = nil },
		"no container":          func(observation *Observation) { observation.ContainerID = "" },
		"no start time":         func(observation *Observation) { observation.StartedAt = time.Time{} },
		"time reversed":         func(observation *Observation) { observation.FinishedAt = observation.StartedAt.Add(-time.Second) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			manifest := testManifest()
			binding, observations := testEvidence(manifest)
			mutate(&observations[3])
			if got := Evaluate(manifest, binding, observations); got.Conclusion != UnableToVerify {
				t.Fatalf("incomplete evidence produced %s", got.Conclusion)
			}
		})
	}
}

func TestFrozenInputs(t *testing.T) {
	manifest := testManifest()
	binding, observations := testEvidence(manifest)
	manifest.Checks[0].Command = []string{"/checks/changed"}
	if got := Evaluate(manifest, binding, observations); got.Conclusion != UnableToVerify {
		t.Fatalf("changed checks reused evidence: %s", got.Conclusion)
	}
	manifest = testManifest()
	manifest.Checks[0].ID = manifest.Checks[1].ID
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("duplicate check ID accepted")
	}
	manifest = testManifest()
	manifest.Files = []FrozenFile{{Path: "check.sh", Content: []byte("changed"), Digest: Digest([]byte("original"))}}
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("changed frozen file accepted")
	}
}

func TestManifestUTF8(test *testing.T) {
	newManifest := func() Manifest {
		manifest := testManifest()
		manifest.Problem = "valid replacement character \ufffd and non-ASCII text \u00e9"
		manifest.Gaps = []string{"remaining cases"}
		manifest.Environment.Variables = map[string]string{"LANG": "C.UTF-8"}
		manifest.Environment.Dependencies = map[string]string{"fixture": "v1"}
		manifest.Environment.Profile = LocalServices
		manifest.Environment.Services = []Service{{ID: "fixture", Command: []string{"/checks/service", "start"}, Port: 8080, ReadyOutput: "ready\n"}}
		content := []byte{0xff, 0x00, 0xfe}
		manifest.Files = []FrozenFile{{Path: "fixture.bin", Content: content, Digest: Digest(content)}}
		for index := range manifest.Checks {
			manifest.Checks[index].Healthy.Services = map[string]string{"fixture": "ready\n"}
			manifest.Checks[index].Failure.Services = map[string]string{"fixture": "ready\n"}
		}
		return manifest
	}
	test.Run("valid strings and binary content survive JSON", func(test *testing.T) {
		manifest := newManifest()
		if err := ValidateManifest(manifest); err != nil {
			test.Fatal(err)
		}
		digest, err := ManifestDigest(manifest)
		if err != nil {
			test.Fatal(err)
		}
		content, err := json.Marshal(manifest)
		if err != nil {
			test.Fatal(err)
		}
		var decoded Manifest
		if err := json.Unmarshal(content, &decoded); err != nil {
			test.Fatal(err)
		}
		decodedDigest, err := ManifestDigest(decoded)
		if err != nil || decodedDigest != digest {
			test.Fatalf("manifest changed on JSON round trip: digest %s, error %v", decodedDigest, err)
		}
	})
	invalid := "\xff"
	mutations := map[string]func(*Manifest){
		"problem":          func(manifest *Manifest) { manifest.Problem = invalid },
		"scope":            func(manifest *Manifest) { manifest.Scope[0] = invalid },
		"gap":              func(manifest *Manifest) { manifest.Gaps[0] = invalid },
		"repository":       func(manifest *Manifest) { manifest.Sources.Repository = invalid },
		"original commit":  func(manifest *Manifest) { manifest.Sources.Original.Commit = invalid },
		"original tree":    func(manifest *Manifest) { manifest.Sources.Original.Tree = invalid },
		"original archive": func(manifest *Manifest) { manifest.Sources.Original.ArchiveDigest = invalid },
		"patched commit":   func(manifest *Manifest) { manifest.Sources.Patched.Commit = invalid },
		"patched tree":     func(manifest *Manifest) { manifest.Sources.Patched.Tree = invalid },
		"patched archive":  func(manifest *Manifest) { manifest.Sources.Patched.ArchiveDigest = invalid },
		"patch digest": func(manifest *Manifest) {
			manifest.Sources.Patched.Commit = ""
			manifest.Sources.PatchDigest = invalid
		},
		"diff digest":            func(manifest *Manifest) { manifest.Sources.DiffDigest = invalid },
		"image name":             func(manifest *Manifest) { manifest.Environment.Image = invalid + "@" + Digest(nil) },
		"image ID":               func(manifest *Manifest) { manifest.Environment.ImageID = invalid },
		"platform":               func(manifest *Manifest) { manifest.Environment.Platform = invalid },
		"profile":                func(manifest *Manifest) { manifest.Environment.Profile = invalid },
		"environment key":        func(manifest *Manifest) { manifest.Environment.Variables[invalid] = "value" },
		"environment value":      func(manifest *Manifest) { manifest.Environment.Variables["LANG"] = invalid },
		"dependency key":         func(manifest *Manifest) { manifest.Environment.Dependencies[invalid] = "value" },
		"dependency value":       func(manifest *Manifest) { manifest.Environment.Dependencies["fixture"] = invalid },
		"service ID":             func(manifest *Manifest) { manifest.Environment.Services[0].ID = invalid },
		"service executable":     func(manifest *Manifest) { manifest.Environment.Services[0].Command[0] = invalid },
		"service argument":       func(manifest *Manifest) { manifest.Environment.Services[0].Command[1] = invalid },
		"service readiness":      func(manifest *Manifest) { manifest.Environment.Services[0].ReadyOutput = invalid },
		"file path":              func(manifest *Manifest) { manifest.Files[0].Path = invalid },
		"file digest":            func(manifest *Manifest) { manifest.Files[0].Digest = invalid },
		"check ID":               func(manifest *Manifest) { manifest.Checks[0].ID = invalid },
		"check kind":             func(manifest *Manifest) { manifest.Checks[0].Kind = invalid },
		"check executable":       func(manifest *Manifest) { manifest.Checks[0].Command[0] = invalid },
		"check argument":         func(manifest *Manifest) { manifest.Checks[0].Command[1] = invalid },
		"check stdin":            func(manifest *Manifest) { manifest.Checks[0].Stdin = invalid },
		"healthy stdout":         func(manifest *Manifest) { manifest.Checks[0].Healthy.Stdout = invalid },
		"failure stdout":         func(manifest *Manifest) { manifest.Checks[0].Failure.Stdout = invalid },
		"healthy service key":    func(manifest *Manifest) { manifest.Checks[0].Healthy.Services[invalid] = "value" },
		"failure service key":    func(manifest *Manifest) { manifest.Checks[0].Failure.Services[invalid] = "value" },
		"healthy service output": func(manifest *Manifest) { manifest.Checks[0].Healthy.Services["fixture"] = invalid },
		"failure service output": func(manifest *Manifest) { manifest.Checks[0].Failure.Services["fixture"] = invalid },
	}
	for name, mutate := range mutations {
		test.Run(name, func(test *testing.T) {
			manifest := newManifest()
			mutate(&manifest)
			if err := ValidateManifest(manifest); err == nil {
				test.Error("invalid UTF-8 accepted by manifest validation")
			}
			if digest, err := ManifestDigest(manifest); err == nil || digest != "" {
				test.Errorf("invalid UTF-8 produced manifest digest %q, error %v", digest, err)
			}
		})
	}
}
