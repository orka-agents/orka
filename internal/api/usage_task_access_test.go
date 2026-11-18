package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestUsageTaskAccessPRLinkIdentity(t *testing.T) {
	f := newExternalAuthorizationFixture(t)
	for _, name := range []string{"public", "private"} {
		require.NoError(t, f.kube.Create(t.Context(), &gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: name, UID: types.UID(name + "-uid"),
		}}))
	}
	f.allowOnly(t, authorizationv1.ResourceAttributes{
		Namespace: "default", Group: gatewayv1alpha1.GroupVersion.Group, Resource: "gateways", Verb: "get", Name: "public",
	})
	public := &store.UsageGatewayOwner{Namespace: "default", NamespaceUID: "namespace-uid", Name: "public", UID: "public-uid"}
	private := &store.UsageGatewayOwner{Namespace: "default", NamespaceUID: "namespace-uid", Name: "private", UID: "private-uid"}
	asOf := time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC)
	data := store.UsageData{
		Works: []store.UsageWorkRequest{
			{ID: "review-a"}, {ID: "visible-direct"}, {ID: "review-b"}, {ID: "issue-direct"}, {ID: "negative-direct"},
		},
		Tasks: []store.UsageTask{
			{Namespace: "default", NamespaceUID: "namespace-uid", TaskUID: "public-task", WorkID: "visible-direct", Repository: "org/repo", PRNumber: 42, GatewayOwner: public},
			{Namespace: "default", NamespaceUID: "namespace-uid", TaskUID: "private-a", WorkID: "review-a", Repository: "org/repo", PRNumber: 42, GatewayOwner: private},
			{Namespace: "default", NamespaceUID: "namespace-uid", TaskUID: "private-b", WorkID: "review-b", Repository: "ORG/REPO", PRNumber: 42, GatewayOwner: private},
			{Namespace: "default", NamespaceUID: "namespace-uid", TaskUID: "private-issue", WorkID: "issue-direct", Repository: "org/repo", GatewayOwner: private},
			{Namespace: "default", NamespaceUID: "namespace-uid", TaskUID: "private-negative", WorkID: "negative-direct", Repository: "org/repo", PRNumber: -1, GatewayOwner: private},
			{Namespace: "default", NamespaceUID: "namespace-uid", TaskUID: "private-unicode", Repository: "org/σ", PRNumber: 42, GatewayOwner: private},
			{Namespace: "default", NamespaceUID: "namespace-uid", TaskUID: "unowned"},
		},
		Observations: []store.UsageObservation{
			{Namespace: "default", TaskUID: "unowned"}, {Namespace: "default", TaskUID: "missing"}, {Namespace: "default"},
		},
		HiddenTasks: map[string]bool{"stale": true},
	}
	wantWorks := []string{"visible-direct"}
	for _, link := range []struct {
		id, namespace, namespaceUID, repository string
		number                                  int64
		linkedAt                                time.Time
		hidden                                  bool
	}{
		{"shared-issue-a", "default", "namespace-uid", "org/repo", 42, asOf.Add(-time.Hour), true},
		{"shared-issue-b", "default", "namespace-uid", "ORG/REPO", 42, asOf, true},
		{"other-namespace", "other", "namespace-uid", "org/repo", 42, asOf, false},
		{"older-namespace", "default", "old-namespace-uid", "org/repo", 42, asOf, false},
		{"other-repository", "default", "namespace-uid", "org/other", 42, asOf, false},
		{"other-pr", "default", "namespace-uid", "org/repo", 43, asOf, false},
		{"future-link", "default", "namespace-uid", "org/repo", 42, asOf.Add(time.Nanosecond), false},
		{"zero-pr", "default", "namespace-uid", "org/repo", 0, asOf, false},
		{"negative-pr", "default", "namespace-uid", "org/repo", -1, asOf, false},
		{"unicode-fold", "default", "namespace-uid", "ORG/ς", 42, asOf, true},
	} {
		data.Works = append(data.Works, store.UsageWorkRequest{ID: link.id})
		data.Links = append(data.Links, store.UsagePRLink{
			Namespace: link.namespace, NamespaceUID: link.namespaceUID, WorkID: link.id,
			Repository: link.repository, Number: link.number, LinkedAt: link.linkedAt,
		})
		if !link.hidden {
			wantWorks = append(wantWorks, link.id)
		}
	}
	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			AuthType: AuthTypeTokenReview, Username: f.user.Username, UID: f.user.UID, Groups: f.user.Groups, Extra: f.user.Extra,
		})
		if err := f.server.handlers.filterUsageTaskAccess(c, &data, asOf); err != nil {
			return err
		}
		return c.SendStatus(fiber.StatusNoContent)
	})
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	gotWorks := make([]string, len(data.Works))
	for i, work := range data.Works {
		gotWorks[i] = work.ID
	}
	require.Equal(t, wantWorks, gotWorks)
	require.Equal(t, map[string]bool{
		"default/private-a": true, "default/private-b": true, "default/private-issue": true,
		"default/private-negative": true, "default/private-unicode": true, "default/missing": true,
	}, data.HiddenTasks)
	require.Len(t, f.reviews, 2, "authorize each Gateway identity once")
}

func TestUsageRepositoryFoldKey(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		equal bool
	}{
		{"Org/Repo", "org/repo", true},
		{"org/σ", "org/ς", true},
		{"org/k", "org/K", true},
		{"org/ß", "org/ẞ", true},
		{"org/ß", "org/ss", false},
		{"org/i", "org/ı", false},
		{"org/é", "org/e\u0301", false},
	} {
		t.Run(tc.a+"/"+tc.b, func(t *testing.T) {
			require.Equal(t, tc.equal, usageRepositoryFoldKey(tc.a) == usageRepositoryFoldKey(tc.b))
		})
	}
}
