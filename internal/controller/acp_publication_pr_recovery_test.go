package controller

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/publisher"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
)

func TestPersistedPullRequestRequestResumesOriginalExpiredEffect(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "session-bound"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			controlStore, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "pr-effect.db"))
			defer closeStore()
			request := publicationPRRecoveryTestRequest()
			original := request
			if legacy {
				original.Intent.SessionUID = ""
			}
			identity := publicationPRRecoveryTestIdentity(request)
			digest, err := acpDomainDigest("external-effect-request", map[string]any{"identity": identity, "request": original})
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now().UTC().Add(-10 * time.Minute)
			effect, err := controlStore.ReserveExternalEffect(ctx, store.ReserveExternalEffectRequest{
				Identity: identity, RequestDigest: digest, Fence: fence, CreatedAt: started,
			})
			if err != nil {
				t.Fatal(err)
			}
			expiry := started.Add(time.Minute)
			effect, err = controlStore.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
				ID: effect.ID, Fence: fence, ExpectedVersion: effect.Version, ExpectedState: effect.State,
				NewState: store.ExternalEffectInFlight, RequestDigest: digest, LeaseOwner: "original-controller",
				LeaseExpiresAt: &expiry, UpdatedAt: started,
			})
			if err != nil {
				t.Fatal(err)
			}
			intents := make(chan publisher.PullRequestIntent, 2)
			server := newDispatcherPublisherServer(t, strings.Repeat("b", 40), original.Intent.ExpectedHeadOID,
				testControlDigestForDispatcher("pr-bundle"), dispatcherPublisherServerOptions{
					inspectPullRequest: func(intent publisher.PullRequestIntent) { intents <- intent },
				})
			defer server.Close()
			client, err := publisherservice.NewClient(publisherservice.ClientConfig{
				BaseURL: server.URL, BearerToken: []byte(strings.Repeat("b", 32)),
				CapabilitySecret: []byte(strings.Repeat("c", 32)), HTTPClient: server.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			dispatcher := &ACPDispatcher{Store: controlStore, Publisher: client}
			selected, err := dispatcher.persistedPullRequestRequest(ctx, request)
			if err != nil || !reflect.DeepEqual(selected, original) {
				t.Fatalf("reconstructed request differs from the original: %v", err)
			}
			response, err := runACPExternalEffect(ctx, dispatcher, fence, identity, selected,
				func(ctx context.Context) (publisherservice.PullRequestReconcileResponse, error) {
					return dispatcher.Publisher.ReconcilePullRequest(ctx, selected)
				})
			if err != nil {
				t.Fatal(err)
			}
			originalKey, err := original.Intent.Key()
			if err != nil || response.Receipt.IntentKey != originalKey || response.OperationID != original.Metadata.OperationID {
				t.Fatalf("recovered receipt did not retain the original identity: %v", err)
			}
			if len(intents) != 1 || !reflect.DeepEqual(<-intents, original.Intent) {
				t.Fatal("publisher did not receive exactly one original intent")
			}
			after, err := controlStore.GetExternalEffect(ctx, effect.ID)
			if err != nil || after.RequestDigest != digest || after.Identity != identity || after.Attempts != 2 ||
				after.State != store.ExternalEffectSucceeded {
				t.Fatalf("recovery did not settle the original effect: %v", err)
			}
		})
	}
}

func TestPersistedPullRequestRequestRejectsChangedLegacyContent(t *testing.T) {
	ctx := context.Background()
	controlStore, fence := newBranchClaimReclamationStore(t)
	request := publicationPRRecoveryTestRequest()
	legacy := request
	legacy.Intent.SessionUID = ""
	identity := publicationPRRecoveryTestIdentity(request)
	digest, err := acpDomainDigest("external-effect-request", map[string]any{"identity": identity, "request": legacy})
	if err != nil {
		t.Fatal(err)
	}
	original, err := controlStore.ReserveExternalEffect(ctx, store.ReserveExternalEffectRequest{
		Identity: identity, RequestDigest: digest, Fence: fence, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &ACPDispatcher{Store: controlStore}
	for _, test := range []struct {
		name   string
		mutate func(*publisherservice.PullRequestReconcileRequest)
	}{
		{name: "base repository", mutate: func(r *publisherservice.PullRequestReconcileRequest) { r.Intent.BaseRepository.ID += "-other" }},
		{name: "head repository URL", mutate: func(r *publisherservice.PullRequestReconcileRequest) { r.Intent.HeadRepository.URL += "/other" }},
		{name: "head ref", mutate: func(r *publisherservice.PullRequestReconcileRequest) { r.Intent.HeadRef += "-other" }},
		{name: "head commit", mutate: func(r *publisherservice.PullRequestReconcileRequest) {
			r.Intent.ExpectedHeadOID = strings.Repeat("b", 40)
		}},
		{name: "generation", mutate: func(r *publisherservice.PullRequestReconcileRequest) { r.Intent.PublicationGeneration++ }},
		{name: "credential identity", mutate: func(r *publisherservice.PullRequestReconcileRequest) {
			r.CredentialRef = &publisherservice.CredentialReference{Name: "different-credential"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := request
			test.mutate(&changed)
			if _, err := dispatcher.persistedPullRequestRequest(ctx, changed); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("changed request = %v, want immutable request conflict", err)
			}
		})
	}
	after, err := controlStore.GetExternalEffect(ctx, original.ID)
	if err != nil || !reflect.DeepEqual(original, after) {
		t.Fatalf("request mismatch changed the original effect: %v", err)
	}
}

func TestPersistedPullRequestRequestKeepsFreshSessionOwnership(t *testing.T) {
	controlStore, _ := newBranchClaimReclamationStore(t)
	request := publicationPRRecoveryTestRequest()
	dispatcher := &ACPDispatcher{Store: controlStore}
	selected, err := dispatcher.persistedPullRequestRequest(context.Background(), request)
	if err != nil || !reflect.DeepEqual(request, selected) || selected.Intent.SessionUID == "" {
		t.Fatalf("fresh publication lost its Session ownership: %v", err)
	}
	id, err := publicationPRRecoveryTestIdentity(request).CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlStore.GetExternalEffect(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("request selection reserved an effect: %v", err)
	}
}

func TestPersistedPullRequestRequestRequiresExactEffectRead(t *testing.T) {
	request := publicationPRRecoveryTestRequest()
	legacy := request
	legacy.Intent.SessionUID = ""
	identity := publicationPRRecoveryTestIdentity(request)
	id, err := identity.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := acpDomainDigest("external-effect-request", map[string]any{"identity": identity, "request": legacy})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*store.ExternalEffect)
		getErr error
		absent bool
	}{
		{name: "exact original"},
		{name: "wrong ID", mutate: func(e *store.ExternalEffect) { e.ID += "-other" }},
		{name: "wrong kind", mutate: func(e *store.ExternalEffect) { e.Identity.Kind += "-other" }},
		{name: "wrong namespace", mutate: func(e *store.ExternalEffect) { e.Identity.Namespace += "-other" }},
		{name: "wrong publication", mutate: func(e *store.ExternalEffect) { e.Identity.AggregateID += "-other" }},
		{name: "wrong operation", mutate: func(e *store.ExternalEffect) { e.Identity.OperationID += "-other" }},
		{name: "missing object", absent: true},
		{name: "lookup failed", getErr: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			effect := &store.ExternalEffect{ID: id, Identity: identity, RequestDigest: digest}
			if test.mutate != nil {
				test.mutate(effect)
			}
			if test.absent {
				effect = nil
			}
			reader := &publicationPRIdentityReader{effect: effect, err: test.getErr}
			dispatcher := &ACPDispatcher{Store: reader}
			selected, err := dispatcher.persistedPullRequestRequest(context.Background(), request)
			switch {
			case test.getErr != nil:
				if !errors.Is(err, test.getErr) {
					t.Fatalf("lookup failure became %v", err)
				}
			case test.mutate != nil || test.absent:
				if !errors.Is(err, store.ErrConflict) {
					t.Fatalf("mismatched effect became %v, want conflict", err)
				}
			default:
				if err != nil || !reflect.DeepEqual(selected, legacy) {
					t.Fatalf("exact original lookup did not preserve legacy request: %v", err)
				}
			}
			if reader.reads != 1 || reader.requested != identity {
				t.Fatal("lookup did not use the exact external-effect identity reader")
			}
		})
	}
}

type publicationPRIdentityReader struct {
	store.DurableControlStore
	effect    *store.ExternalEffect
	err       error
	reads     int
	requested store.ExternalEffectIdentity
}

func (s *publicationPRIdentityReader) GetExternalEffectByIdentity(_ context.Context, identity store.ExternalEffectIdentity) (*store.ExternalEffect, error) {
	s.reads++
	s.requested = identity
	return s.effect, s.err
}

func publicationPRRecoveryTestRequest() publisherservice.PullRequestReconcileRequest {
	return publisherservice.PullRequestReconcileRequest{
		Metadata: publisherservice.OperationMetadata{Namespace: "default", PublicationID: "original-publication", OperationID: "original-pr-operation"},
		Intent: publisher.PullRequestIntent{
			BaseRepository: publisher.Repository{Provider: "github", ID: "github.com/orka/base", URL: "https://github.com/orka/base.git"},
			BaseRef:        "refs/heads/main",
			HeadRepository: publisher.Repository{Provider: "github", ID: "github.com/orka/fork", URL: "https://github.com/orka/fork.git"},
			HeadRef:        "refs/heads/orka/session-original", PublicationGeneration: 1, ExpectedHeadOID: strings.Repeat("a", 40),
			SessionUID: "original-publication-session",
		},
	}
}

func publicationPRRecoveryTestIdentity(request publisherservice.PullRequestReconcileRequest) store.ExternalEffectIdentity {
	return store.ExternalEffectIdentity{
		Kind: "publisher.pull-request", Namespace: request.Metadata.Namespace,
		AggregateID: request.Metadata.PublicationID, OperationID: request.Metadata.OperationID,
	}
}

func TestRecoverPublicationPullRequestPreservesLegacyCommittedEffect(t *testing.T) {
	ctx := context.Background()
	controlStore, fence := newBranchClaimReclamationStore(t)
	task := branchClaimReclamationTask("legacy-pr-task", "legacy-pr-prompt")
	base := publisher.Repository{Provider: "github", ID: "github.com/orka/base", URL: "https://github.com/orka/base.git"}
	head := publisher.Repository{Provider: "github", ID: "github.com/orka/fork", URL: "https://github.com/orka/fork.git"}
	publication := &store.Publication{
		ID: publicationIDForTask(task), Namespace: task.Namespace, Generation: 1, Version: 4,
		SessionUID: "original-publication-session", State: store.PublicationVerifying,
		PreparedReceipt: &store.PreparedPublicationReceipt{CommitSHA: strings.Repeat("a", 40)},
		PRIntent: &store.PullRequestIntent{
			BaseRepositoryID: base.ID, BaseRef: "refs/heads/main", HeadRepositoryID: head.ID,
			HeadRef: "refs/heads/orka/original-session", PublicationGeneration: 1, ExpectedHeadSHA: strings.Repeat("a", 40),
		},
	}
	operation := publicationOperationID("pr-reconcile", task)
	identity := store.ExternalEffectIdentity{
		Kind: "publisher.pull-request", Namespace: task.Namespace, AggregateID: publication.ID, OperationID: operation,
	}
	// The previous controller persisted this exact request without sessionUid,
	// although the Publication already had its immutable Session owner.
	legacy := publisherservice.PullRequestReconcileRequest{
		Metadata: publisherservice.OperationMetadata{Namespace: task.Namespace, PublicationID: publication.ID, OperationID: operation},
		Intent: publisher.PullRequestIntent{
			BaseRepository: base, BaseRef: publication.PRIntent.BaseRef, HeadRepository: head, HeadRef: publication.PRIntent.HeadRef,
			PublicationGeneration: 1, ExpectedHeadOID: publication.PRIntent.ExpectedHeadSHA,
		},
	}
	legacyKey, err := legacy.Intent.Key()
	if err != nil {
		t.Fatal(err)
	}
	response := publisherservice.PullRequestReconcileResponse{
		OperationID: operation, RequestDigest: testControlDigestForDispatcher("legacy-pr-request"),
		Receipt: publisher.PullRequestReceipt{
			IntentKey: legacyKey, ForgeID: "github:101:42", URL: "https://github.com/orka/base/pull/42",
			State: publisher.PullRequestOpen, HeadOID: publication.PRIntent.ExpectedHeadSHA,
		},
	}
	dispatcher := &ACPDispatcher{Store: controlStore}
	if _, err := runACPExternalEffect(ctx, dispatcher, fence, identity, legacy,
		func(context.Context) (publisherservice.PullRequestReconcileResponse, error) { return response, nil }); err != nil {
		t.Fatal(err)
	}
	effectID, err := identity.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	before, err := controlStore.GetExternalEffect(ctx, effectID)
	if err != nil {
		t.Fatal(err)
	}
	projection := &publicationPRRecoveryReceiptStore{DurableControlStore: controlStore, publication: publication}
	dispatcher.Store = projection
	// Publisher is intentionally absent: recovery must consume the durable
	// original response, without another forge request or a new intent key.
	verification := &store.PublicationVerificationReceipt{Outcome: store.PublicationVerifiedExact}
	got, gotVerification, reason, err := dispatcher.recoverPublicationPullRequest(ctx, publication, verification, "verify-original",
		persistedPublicationRecoveryContext{task: task, fence: fence, pullRequestBase: base, target: head})
	if err != nil || reason != "" || gotVerification != verification {
		t.Fatalf("legacy committed PR recovery = reason %q, error %v", reason, err)
	}
	if projection.writes != 1 || got.PullRequestReceipt == nil || got.PullRequestReceipt.IntentKey != legacyKey ||
		got.PullRequestReceipt.OperationID != operation || got.PullRequestReceipt.RequestDigest != response.RequestDigest {
		t.Fatalf("recovery did not project the original PR receipt: %#v", got.PullRequestReceipt)
	}
	after, err := controlStore.GetExternalEffect(ctx, effectID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("recovery changed the committed external effect: %v", err)
	}
}

type publicationPRRecoveryReceiptStore struct {
	store.DurableControlStore
	publication *store.Publication
	writes      int
}

func (s *publicationPRRecoveryReceiptStore) SetPublicationPRReceipt(_ context.Context, request store.SetPublicationPRReceiptRequest) (*store.Publication, error) {
	s.writes++
	result := *s.publication
	result.PullRequestReceipt = &request.Receipt
	return &result, nil
}
