package patchverification

import (
	"testing"
	"time"
)

func matchedHTTPObservation(check Check, expectation Expectation) Observation {
	exit := 0
	return Observation{
		Side: Original, CheckID: check.ID, ContainerID: "container",
		StartedAt: time.Unix(1, 0), FinishedAt: time.Unix(2, 0),
		Executed: true, HTTPCompleted: true, ExitCode: &exit,
		StdoutDigest: Digest([]byte(expectation.Stdout)), StdoutBytes: len(expectation.Stdout),
		StderrDigest: Digest(nil),
	}
}

func TestMatchedHTTPOutputUsesOnlyExactFrozenBytes(t *testing.T) {
	check := httpCheck(t)
	for _, expectation := range []Expectation{check.Healthy, check.Failure} {
		observation := matchedHTTPObservation(check, expectation)
		output, matched := MatchedHTTPOutput(check, observation)
		if !matched || output != expectation.Stdout ||
			len(output) != observation.StdoutBytes || Digest([]byte(output)) != observation.StdoutDigest {
			t.Fatal("exact protected observation was not reconstructed from its frozen bytes")
		}
	}
}

func TestMatchedHTTPOutputOmitsUnusableOrUnmatchedEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*Check, *Observation){
		"other check": func(_ *Check, observation *Observation) { observation.CheckID = "other" },
		"command only": func(check *Check, _ *Observation) {
			check.Command = check.HTTP.ServerCommand
			check.HTTP = nil
		},
		"incomplete HTTP": func(_ *Check, observation *Observation) { observation.HTTPCompleted = false },
		"not executed":    func(_ *Check, observation *Observation) { observation.Executed = false },
		"missing exit":    func(_ *Check, observation *Observation) { observation.ExitCode = nil },
		"different exit": func(_ *Check, observation *Observation) {
			exit := 1
			observation.ExitCode = &exit
		},
		"unmatched digest": func(_ *Check, observation *Observation) {
			observation.StdoutDigest = Digest([]byte("unknown response"))
		},
		"unmatched length": func(_ *Check, observation *Observation) { observation.StdoutBytes++ },
		"no container":     func(_ *Check, observation *Observation) { observation.ContainerID = "" },
		"no start":         func(_ *Check, observation *Observation) { observation.StartedAt = time.Time{} },
		"invalid finish":   func(_ *Check, observation *Observation) { observation.FinishedAt = time.Time{} },
		"timed out":        func(_ *Check, observation *Observation) { observation.TimedOut = true },
		"skipped":          func(_ *Check, observation *Observation) { observation.Skipped = true },
		"truncated":        func(_ *Check, observation *Observation) { observation.OutputTruncated = true },
		"setup failed":     func(_ *Check, observation *Observation) { observation.SetupError = "unavailable" },
		"invalid stderr":   func(_ *Check, observation *Observation) { observation.StderrDigest = "" },
		"extra service": func(_ *Check, observation *Observation) {
			observation.ServiceOutputs = map[string]CapturedOutput{"unexpected": {Digest: Digest(nil)}}
		},
		"noncanonical expectation": func(check *Check, _ *Observation) { check.Healthy.Stdout += " " },
		"identical expectations":   func(check *Check, _ *Observation) { check.Failure = check.Healthy },
	} {
		t.Run(name, func(t *testing.T) {
			check := httpCheck(t)
			observation := matchedHTTPObservation(check, check.Healthy)
			mutate(&check, &observation)
			if output, matched := MatchedHTTPOutput(check, observation); matched || output != "" {
				t.Fatal("unusable or unmatched evidence gained readable output")
			}
		})
	}
}
