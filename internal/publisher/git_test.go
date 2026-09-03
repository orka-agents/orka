package publisher

import (
	"context"
	"strings"
	"testing"
)

func TestPublisherObserveRefPeelsAnnotatedTags(t *testing.T) {
	fixture := newRepositoryFixture(t, false)
	const (
		annotatedTag      = "v1.2.3-annotated"
		annotatedTagRef   = "refs/tags/" + annotatedTag
		lightweightTag    = "v1.2.3-lightweight"
		lightweightTagRef = "refs/tags/" + lightweightTag
	)
	runTestGit(t, fixture.seed, fixedCommitEnvironment(), "tag", "-a", annotatedTag, "-m", "annotated release")
	runTestGit(t, fixture.seed, nil, "tag", lightweightTag)
	runTestGit(t, fixture.seed, nil, "push", "--", fixture.source.URL, annotatedTagRef, lightweightTagRef)

	annotatedTagOID := strings.TrimSpace(runTestGit(t, fixture.seed, nil, "rev-parse", annotatedTagRef))
	if annotatedTagOID == fixture.baselineOID {
		t.Fatal("annotated tag fixture unexpectedly resolves directly to the commit object")
	}

	publisher := newTestPublisher(t)
	box, err := publisher.newSandbox("observe-ref-test")
	if err != nil {
		t.Fatalf("create publisher sandbox: %v", err)
	}
	defer func() {
		if err := box.Close(); err != nil {
			t.Errorf("close publisher sandbox: %v", err)
		}
	}()

	for _, test := range []struct {
		name string
		ref  string
	}{
		{name: "branch", ref: "refs/heads/main"},
		{name: "lightweight tag", ref: lightweightTagRef},
		{name: "annotated tag", ref: annotatedTagRef},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed, err := publisher.observeRef(context.Background(), box, fixture.source, test.ref)
			if err != nil {
				t.Fatalf("observeRef(%q): %v", test.ref, err)
			}
			if observed.Absent || observed.OID != fixture.baselineOID {
				t.Fatalf("observeRef(%q) = %#v, want commit %s", test.ref, observed, fixture.baselineOID)
			}
		})
	}
}

func TestPublisherObserveSourcePreservesExactObjectID(t *testing.T) {
	fixture := newRepositoryFixture(t, false)
	publisher := newTestPublisher(t)
	box, err := publisher.newSandbox("observe-source-test")
	if err != nil {
		t.Fatalf("create publisher sandbox: %v", err)
	}
	defer func() {
		if err := box.Close(); err != nil {
			t.Errorf("close publisher sandbox: %v", err)
		}
	}()

	observed, err := publisher.observeSource(context.Background(), box, fixture.source, fixture.baselineOID)
	if err != nil {
		t.Fatalf("observeSource(%q): %v", fixture.baselineOID, err)
	}
	if observed.Absent || observed.OID != fixture.baselineOID {
		t.Fatalf("observeSource(%q) = %#v, want exact commit", fixture.baselineOID, observed)
	}
}

func TestPrepareAcceptsAnnotatedSourceTag(t *testing.T) {
	fixture := newRepositoryFixture(t, false)
	const (
		tag    = "v2.0.0"
		tagRef = "refs/tags/" + tag
	)
	runTestGit(t, fixture.seed, fixedCommitEnvironment(), "tag", "-a", tag, "-m", "release")
	runTestGit(t, fixture.seed, nil, "push", "--", fixture.source.URL, tagRef)

	request := fixture.prepareRequest("publication-annotated-source", "prepare-annotated-source", RemoteRef{Absent: true})
	request.SourceRef = tagRef
	prepared, err := newTestPublisher(t).Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare annotated source tag: %v", err)
	}
	if prepared.SourceRef != tagRef || prepared.BaselineOID != fixture.baselineOID {
		t.Fatalf("prepared annotated source = %#v, want ref %q at %s", prepared, tagRef, fixture.baselineOID)
	}
}
