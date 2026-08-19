/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const cacheUnsupportedContinue = "continue-not-supported"

type recordedListCall struct {
	Kind            string
	Namespace       string
	Limit           int64
	Continue        string
	ResourceVersion string
}

type listCallRecorder struct {
	mu    sync.Mutex
	calls []recordedListCall
}

func (r *listCallRecorder) record(kind string, opts client.ListOptions) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedListCall{
		Kind:            kind,
		Namespace:       opts.Namespace,
		Limit:           opts.Limit,
		Continue:        opts.Continue,
		ResourceVersion: listResourceVersion(opts),
	})
}

func (r *listCallRecorder) snapshot() []recordedListCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedListCall(nil), r.calls...)
}

func newCachePaginationClient(t *testing.T, objects ...client.Object) (client.Client, *listCallRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	recorder := &listCallRecorder{}
	cacheClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				listOpts := client.ListOptions{}
				listOpts.ApplyOptions(opts)
				recorder.record(listKind(list), listOpts)
				if listOpts.Continue != "" {
					return fmt.Errorf("cache continuation is unsupported")
				}
				if err := underlying.List(ctx, list, opts...); err != nil {
					return err
				}
				if listOpts.Limit > 0 {
					list.SetContinue(cacheUnsupportedContinue)
				}
				return nil
			},
		}).
		Build()
	return cacheClient, recorder
}

type paginationTestCursor struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Offset    int    `json:"offset"`
}

type paginatedAPIReader struct {
	mu        sync.Mutex
	getReader client.Reader
	tasks     []corev1alpha1.Task
	agents    []corev1alpha1.Agent
	skills    []corev1alpha1.Skill
	tools     []corev1alpha1.Tool
	calls     []recordedListCall
}

func (r *paginatedAPIReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if r.getReader != nil {
		return r.getReader.Get(ctx, key, obj, opts...)
	}
	return apierrors.NewNotFound(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "test"}, key.Name)
}

func (r *paginatedAPIReader) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	listOpts := client.ListOptions{}
	listOpts.ApplyOptions(opts)
	kind := listKind(list)

	r.mu.Lock()
	r.calls = append(r.calls, recordedListCall{
		Kind:            kind,
		Namespace:       listOpts.Namespace,
		Limit:           listOpts.Limit,
		Continue:        listOpts.Continue,
		ResourceVersion: listResourceVersion(listOpts),
	})
	r.mu.Unlock()

	switch typed := list.(type) {
	case *corev1alpha1.TaskList:
		items, meta, err := paginateTestItems(r.tasks, kind, listOpts)
		if err != nil {
			return err
		}
		typed.Items = items
		typed.ListMeta = meta
	case *corev1alpha1.AgentList:
		items, meta, err := paginateTestItems(r.agents, kind, listOpts)
		if err != nil {
			return err
		}
		typed.Items = items
		typed.ListMeta = meta
	case *corev1alpha1.SkillList:
		items, meta, err := paginateTestItems(r.skills, kind, listOpts)
		if err != nil {
			return err
		}
		typed.Items = items
		typed.ListMeta = meta
	case *corev1alpha1.ToolList:
		items, meta, err := paginateTestItems(r.tools, kind, listOpts)
		if err != nil {
			return err
		}
		typed.Items = items
		typed.ListMeta = meta
	default:
		return fmt.Errorf("unsupported list type %T", list)
	}
	return nil
}

func (r *paginatedAPIReader) snapshotCalls() []recordedListCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedListCall(nil), r.calls...)
}

func paginateTestItems[T any](items []T, kind string, opts client.ListOptions) ([]T, metav1.ListMeta, error) {
	const resourceVersion = "test-resource-version"
	if requested := listResourceVersion(opts); requested != "" && requested != resourceVersion {
		return nil, metav1.ListMeta{}, fmt.Errorf("resourceVersion = %q, want %q", requested, resourceVersion)
	}
	start := 0
	if opts.Continue != "" {
		cursor, err := decodePaginationTestCursor(opts.Continue)
		if err != nil {
			return nil, metav1.ListMeta{}, err
		}
		if cursor.Kind != kind || cursor.Namespace != opts.Namespace || cursor.Offset < 0 || cursor.Offset > len(items) {
			return nil, metav1.ListMeta{}, fmt.Errorf("invalid test continuation")
		}
		start = cursor.Offset
	}

	end := len(items)
	if opts.Limit > 0 && int64(end-start) > opts.Limit {
		end = start + int(opts.Limit)
	}
	page := append([]T(nil), items[start:end]...)
	meta := metav1.ListMeta{ResourceVersion: resourceVersion}
	if end < len(items) {
		meta.Continue = encodePaginationTestCursor(paginationTestCursor{
			Kind:      kind,
			Namespace: opts.Namespace,
			Offset:    end,
		})
		remaining := int64(len(items) - end)
		meta.RemainingItemCount = &remaining
	}
	return page, meta, nil
}

func listResourceVersion(opts client.ListOptions) string {
	if opts.Raw == nil {
		return ""
	}
	return opts.Raw.ResourceVersion
}

func encodePaginationTestCursor(cursor paginationTestCursor) string {
	data, err := json.Marshal(cursor)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodePaginationTestCursor(raw string) (paginationTestCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return paginationTestCursor{}, fmt.Errorf("decode test continuation: %w", err)
	}
	var cursor paginationTestCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return paginationTestCursor{}, fmt.Errorf("unmarshal test continuation: %w", err)
	}
	return cursor, nil
}

func listKind(list client.ObjectList) string {
	switch list.(type) {
	case *corev1alpha1.TaskList:
		return "tasks"
	case *corev1alpha1.AgentList:
		return "agents"
	case *corev1alpha1.SkillList:
		return "skills"
	case *corev1alpha1.ToolList:
		return "tools"
	default:
		return fmt.Sprintf("%T", list)
	}
}

type paginationListResponse[T any] struct {
	Items    []T      `json:"items"`
	Metadata ListMeta `json:"metadata"`
}

func listPage[T any](t *testing.T, app *fiber.App, path string) (paginationListResponse[T], int) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	var body paginationListResponse[T]
	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	}
	return body, resp.StatusCode
}

func pathWithContinue(path, continuation string) string {
	parsed, err := url.Parse(path)
	if err != nil {
		panic(err)
	}
	query := parsed.Query()
	query.Set("continue", continuation)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func newListTestApp(handlers *Handlers) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Get("/tasks", handlers.ListTasks)
	app.Get("/agents", handlers.ListAgents)
	app.Get("/skills", handlers.ListSkills)
	app.Get("/tools", handlers.ListTools)
	return app
}

func TestHandlers_ListTasks_CachePaginationReproducesUnsupportedContinuation(t *testing.T) {
	cacheClient, _ := newCachePaginationClient(t, &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: "default"},
	})
	handlers := NewHandlers(HandlersConfig{Client: cacheClient})
	app := newListTestApp(handlers)

	first, status := listPage[corev1alpha1.Task](t, app, "/tasks?limit=1&paginate=true")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, cacheUnsupportedContinue, first.Metadata.Continue)

	_, status = listPage[corev1alpha1.Task](t, app, pathWithContinue("/tasks?limit=1&paginate=true", first.Metadata.Continue))
	require.Equal(t, http.StatusInternalServerError, status)
}

func TestHandlers_ListTasks_APIReaderExposesUISecondPage(t *testing.T) {
	tasks := make([]corev1alpha1.Task, 26)
	objects := make([]client.Object, 0, len(tasks))
	for i := range tasks {
		tasks[i].ObjectMeta = metav1.ObjectMeta{
			Name:      fmt.Sprintf("task-%02d", i+1),
			Namespace: "default",
		}
		objects = append(objects, &tasks[i])
	}
	cacheClient, cacheCalls := newCachePaginationClient(t, objects...)
	apiReader := &paginatedAPIReader{tasks: tasks}
	handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader})
	app := newListTestApp(handlers)

	first, status := listPage[corev1alpha1.Task](t, app, "/tasks?limit=25&paginate=true")
	require.Equal(t, http.StatusOK, status)
	require.Len(t, first.Items, 25)
	require.NotEmpty(t, first.Metadata.Continue)
	require.NotEqual(t, cacheUnsupportedContinue, first.Metadata.Continue)
	require.NotNil(t, first.Metadata.RemainingItemCount)
	require.EqualValues(t, 1, *first.Metadata.RemainingItemCount)

	second, status := listPage[corev1alpha1.Task](t, app, pathWithContinue("/tasks?limit=25&paginate=true", first.Metadata.Continue))
	require.Equal(t, http.StatusOK, status)
	require.Len(t, second.Items, 1)
	require.Equal(t, "task-26", second.Items[0].Name)
	require.Empty(t, second.Metadata.Continue)
	require.Empty(t, cacheCalls.snapshot())
	require.Equal(t, []recordedListCall{
		{Kind: "tasks", Namespace: "default", Limit: 25},
		{Kind: "tasks", Namespace: "default", Limit: 25, Continue: first.Metadata.Continue},
	}, apiReader.snapshotCalls())
}

func TestHandlers_ListTasks_LimitOnlyReturnsCompleteInventory(t *testing.T) {
	tasks := make([]corev1alpha1.Task, 26)
	objects := make([]client.Object, 0, len(tasks))
	for i := range tasks {
		tasks[i].ObjectMeta = metav1.ObjectMeta{
			Name:      fmt.Sprintf("task-%02d", i+1),
			Namespace: "default",
		}
		objects = append(objects, &tasks[i])
	}
	cacheClient, cacheCalls := newCachePaginationClient(t, objects...)
	apiReader := &paginatedAPIReader{tasks: tasks}
	app := newListTestApp(NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader}))

	page, status := listPage[corev1alpha1.Task](t, app, "/tasks?limit=25")
	require.Equal(t, http.StatusOK, status)
	require.Len(t, page.Items, 26)
	require.Empty(t, page.Metadata.Continue)
	require.Nil(t, page.Metadata.RemainingItemCount)
	require.Empty(t, cacheCalls.snapshot())
	require.Equal(t, []recordedListCall{{Kind: "tasks", Namespace: "default"}}, apiReader.snapshotCalls())
}

func TestHandlers_ListTasks_RequiresExplicitPaginationOptIn(t *testing.T) {
	cacheClient, cacheCalls := newCachePaginationClient(t)
	apiReader := &paginatedAPIReader{}
	app := newListTestApp(NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader}))

	for _, path := range []string{
		"/tasks?limit=1&continue=opaque",
		"/tasks?limit=1&paginate=maybe",
		"/tasks?limit=0&paginate=true",
	} {
		_, status := listPage[corev1alpha1.Task](t, app, path)
		require.Equal(t, http.StatusBadRequest, status, "path %q", path)
	}
	require.Empty(t, cacheCalls.snapshot())
	require.Empty(t, apiReader.snapshotCalls())
}

func TestHandlers_ListTasks_GatewayFilteredCallerReceivesCompleteLogicalList(t *testing.T) {
	hiddenGatewayTask := gatewayTaskAccessFixture()
	visibleTasks := []corev1alpha1.Task{
		{ObjectMeta: metav1.ObjectMeta{Name: "visible-task-1", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "visible-task-2", Namespace: "default"}},
	}
	tasks := []corev1alpha1.Task{*hiddenGatewayTask, visibleTasks[0], visibleTasks[1]}
	objects := []client.Object{hiddenGatewayTask, &visibleTasks[0], &visibleTasks[1]}
	cacheClient, cacheCalls := newCachePaginationClient(t, objects...)
	apiReader := &paginatedAPIReader{
		getReader: newGatewayIdentityClient(t, "namespace-uid", "gateway-uid"),
		tasks:     tasks,
	}
	handlers := NewHandlers(HandlersConfig{
		Client:     cacheClient,
		APIReader:  apiReader,
		KubeClient: denyingSubjectAccessReviewClient(t, nil, nil),
	})
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Use(tokenReviewUserMiddleware(limitedTokenReviewUser("default")))
	app.Get("/tasks", handlers.ListTasks)

	path := "/tasks?namespace=default&limit=1&paginate=true"
	page, status := listPage[corev1alpha1.Task](t, app, path)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{"visible-task-1"}, []string{page.Items[0].Name})
	require.NotEmpty(t, page.Metadata.Continue)
	require.NotNil(t, page.Metadata.RemainingItemCount)
	require.EqualValues(t, 1, *page.Metadata.RemainingItemCount)

	second, status := listPage[corev1alpha1.Task](t, app, pathWithContinue(path, page.Metadata.Continue))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{"visible-task-2"}, []string{second.Items[0].Name})
	require.Empty(t, second.Metadata.Continue)
	require.Nil(t, second.Metadata.RemainingItemCount)
	require.Empty(t, cacheCalls.snapshot())
	require.Equal(t, []recordedListCall{
		{Kind: "tasks", Namespace: "default"},
		{Kind: "tasks", Namespace: "default"},
	}, apiReader.snapshotCalls())
}

func TestHandlers_ListTasks_LimitZeroKeepsCachePath(t *testing.T) {
	tasks := []corev1alpha1.Task{
		{ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "task-2", Namespace: "default"}},
	}
	objects := []client.Object{&tasks[0], &tasks[1]}
	cacheClient, cacheCalls := newCachePaginationClient(t, objects...)
	apiReader := &paginatedAPIReader{tasks: tasks}
	handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader})
	app := newListTestApp(handlers)

	page, status := listPage[corev1alpha1.Task](t, app, "/tasks?limit=0")
	require.Equal(t, http.StatusOK, status)
	require.Len(t, page.Items, 2)
	require.Empty(t, page.Metadata.Continue)
	require.Equal(t, []recordedListCall{{Kind: "tasks", Namespace: "default"}}, cacheCalls.snapshot())
	require.Empty(t, apiReader.snapshotCalls())
}

func TestHandlers_ListAgents_APIReaderExposesCLI101stAgent(t *testing.T) {
	agents := make([]corev1alpha1.Agent, 101)
	objects := make([]client.Object, 0, len(agents))
	for i := range agents {
		agents[i].ObjectMeta = metav1.ObjectMeta{
			Name:      fmt.Sprintf("agent-%03d", i+1),
			Namespace: "default",
		}
		objects = append(objects, &agents[i])
	}
	cacheClient, cacheCalls := newCachePaginationClient(t, objects...)
	apiReader := &paginatedAPIReader{agents: agents}
	handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader})
	app := newListTestApp(handlers)

	first, status := listPage[corev1alpha1.Agent](t, app, "/agents?limit=100")
	require.Equal(t, http.StatusOK, status)
	require.Len(t, first.Items, 100)
	require.NotEmpty(t, first.Metadata.Continue)

	second, status := listPage[corev1alpha1.Agent](t, app, pathWithContinue("/agents?limit=100", first.Metadata.Continue))
	require.Equal(t, http.StatusOK, status)
	require.Len(t, second.Items, 1)
	require.Equal(t, "agent-101", second.Items[0].Name)
	require.Empty(t, cacheCalls.snapshot())
	require.Len(t, apiReader.snapshotCalls(), 2)
}

func TestHandlers_ListAgents_OmittedPaginationReturnsCompleteInventory(t *testing.T) {
	agents := make([]corev1alpha1.Agent, 101)
	objects := make([]client.Object, 0, len(agents))
	for i := range agents {
		agents[i].ObjectMeta = metav1.ObjectMeta{Name: fmt.Sprintf("agent-%03d", i+1), Namespace: "default"}
		objects = append(objects, &agents[i])
	}
	cacheClient, cacheCalls := newCachePaginationClient(t, objects...)
	apiReader := &paginatedAPIReader{agents: agents}
	app := newListTestApp(NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader}))

	page, status := listPage[corev1alpha1.Agent](t, app, "/agents")
	require.Equal(t, http.StatusOK, status)
	require.Len(t, page.Items, 101)
	require.Empty(t, page.Metadata.Continue)
	require.Empty(t, cacheCalls.snapshot())
	require.Equal(t, []recordedListCall{{Kind: "agents", Namespace: "default"}}, apiReader.snapshotCalls())
}

type skillListTestItem struct {
	Name string `json:"name"`
}

func TestHandlers_ListSkills_UsesAPIReaderContinuation(t *testing.T) {
	skills := []corev1alpha1.Skill{
		{ObjectMeta: metav1.ObjectMeta{Name: "skill-1", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "skill-2", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "skill-3", Namespace: "default"}},
	}
	objects := []client.Object{&skills[0], &skills[1], &skills[2]}
	cacheClient, cacheCalls := newCachePaginationClient(t, objects...)
	apiReader := &paginatedAPIReader{skills: skills}
	handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader})
	app := newListTestApp(handlers)

	first, status := listPage[skillListTestItem](t, app, "/skills?limit=2")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []skillListTestItem{{Name: "skill-1"}, {Name: "skill-2"}}, first.Items)
	require.NotEmpty(t, first.Metadata.Continue)

	second, status := listPage[skillListTestItem](t, app, pathWithContinue("/skills?limit=2", first.Metadata.Continue))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []skillListTestItem{{Name: "skill-3"}}, second.Items)
	require.Empty(t, cacheCalls.snapshot())
	require.Len(t, apiReader.snapshotCalls(), 2)
}

func TestHandlers_ListSkills_OmittedPaginationReturnsCompleteInventory(t *testing.T) {
	skills := make([]corev1alpha1.Skill, 101)
	objects := make([]client.Object, 0, len(skills))
	for i := range skills {
		skills[i].ObjectMeta = metav1.ObjectMeta{Name: fmt.Sprintf("skill-%03d", i+1), Namespace: "default"}
		objects = append(objects, &skills[i])
	}
	cacheClient, cacheCalls := newCachePaginationClient(t, objects...)
	apiReader := &paginatedAPIReader{skills: skills}
	app := newListTestApp(NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader}))

	page, status := listPage[skillListTestItem](t, app, "/skills")
	require.Equal(t, http.StatusOK, status)
	require.Len(t, page.Items, 101)
	require.Empty(t, page.Metadata.Continue)
	require.Empty(t, cacheCalls.snapshot())
	require.Equal(t, []recordedListCall{{Kind: "skills", Namespace: "default"}}, apiReader.snapshotCalls())
}

type toolListTestItem struct {
	Name    string `json:"name"`
	Builtin bool   `json:"builtin"`
}

func TestHandlers_ListTools_OmittedPaginationReturnsCompleteInventory(t *testing.T) {
	customTools := make([]corev1alpha1.Tool, 101)
	objects := make([]client.Object, 0, len(customTools))
	for i := range customTools {
		customTools[i].ObjectMeta = metav1.ObjectMeta{Name: fmt.Sprintf("custom-%03d", i+1), Namespace: "default"}
		objects = append(objects, &customTools[i])
	}
	cacheClient, cacheCalls := newCachePaginationClient(t, objects...)
	apiReader := &paginatedAPIReader{tools: customTools}
	app := newListTestApp(NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader}))

	page, status := listPage[toolListTestItem](t, app, "/tools")
	require.Equal(t, http.StatusOK, status)
	require.Len(t, page.Items, len(builtinToolsList)+len(customTools))
	require.Empty(t, page.Metadata.Continue)
	require.Empty(t, cacheCalls.snapshot())
	require.Equal(t, []recordedListCall{{Kind: "tools", Namespace: "default"}}, apiReader.snapshotCalls())
}

func TestHandlers_ListTools_CombinedPaginationHonorsLimitAndAllContinuations(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run("limit-"+strconv.Itoa(limit), func(t *testing.T) {
			customTools := []corev1alpha1.Tool{
				{ObjectMeta: metav1.ObjectMeta{Name: "custom-1", Namespace: "default"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "custom-2", Namespace: "default"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "custom-3", Namespace: "default"}},
			}
			objects := []client.Object{&customTools[0], &customTools[1], &customTools[2]}
			cacheClient, cacheCalls := newCachePaginationClient(t, objects...)
			apiReader := &paginatedAPIReader{tools: customTools}
			handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader})
			app := newListTestApp(handlers)

			path := "/tools?limit=" + strconv.Itoa(limit)
			seenCursors := map[string]struct{}{}
			var names []string
			var builtins int
			var remainingCounts []int64
			for pageNumber := 1; pageNumber <= 20; pageNumber++ {
				page, status := listPage[toolListTestItem](t, app, path)
				require.Equal(t, http.StatusOK, status)
				require.LessOrEqual(t, len(page.Items), limit)
				if page.Metadata.RemainingItemCount != nil {
					remainingCounts = append(remainingCounts, *page.Metadata.RemainingItemCount)
				}
				for _, item := range page.Items {
					names = append(names, item.Name)
					if item.Builtin {
						builtins++
					}
				}
				if page.Metadata.Continue == "" {
					break
				}
				_, duplicate := seenCursors[page.Metadata.Continue]
				require.False(t, duplicate, "combined cursor repeated on page %d", pageNumber)
				seenCursors[page.Metadata.Continue] = struct{}{}
				path = pathWithContinue("/tools?limit="+strconv.Itoa(limit), page.Metadata.Continue)
			}

			wantNames := make([]string, 0, len(builtinToolsList)+len(customTools))
			for _, tool := range builtinToolsList {
				wantNames = append(wantNames, tool["name"].(string))
			}
			wantNames = append(wantNames, "custom-1", "custom-2", "custom-3")
			require.Equal(t, wantNames, names)
			require.Equal(t, len(builtinToolsList), builtins)
			require.Contains(t, remainingCounts, int64(1))
			require.Empty(t, cacheCalls.snapshot())

			calls := apiReader.snapshotCalls()
			require.NotEmpty(t, calls)
			for _, call := range calls {
				require.Equal(t, "tools", call.Kind)
				require.Equal(t, "default", call.Namespace)
				require.Zero(t, call.Limit)
				require.Empty(t, call.Continue)
				require.Empty(t, call.ResourceVersion)
			}

		})
	}
}

func TestHandlers_ListTools_RejectsMalformedCombinedCursor(t *testing.T) {
	cacheClient, _ := newCachePaginationClient(t)
	apiReader := &paginatedAPIReader{}
	handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader})
	app := newListTestApp(handlers)

	_, status := listPage[toolListTestItem](t, app, "/tools?limit=1&continue=not-a-combined-cursor")
	require.Equal(t, http.StatusBadRequest, status)
	require.Empty(t, apiReader.snapshotCalls())
}

func TestHandlers_ListTools_RejectsCallerControlledResourceVersion(t *testing.T) {
	cacheClient, _ := newCachePaginationClient(t)
	apiReader := &paginatedAPIReader{}
	app := newListTestApp(NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader}))
	forged := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil,
		`{"v":%d,"n":"default","b":%d,"r":"caller-selected"}`,
		toolListCursorVersion,
		len(builtinToolsList),
	))

	_, status := listPage[toolListTestItem](t, app, pathWithContinue("/tools?limit=1", forged))
	require.Equal(t, http.StatusBadRequest, status)
	require.Empty(t, apiReader.snapshotCalls())
}

func TestHandlers_ListTools_RejectsLimitZero(t *testing.T) {
	cacheClient, _ := newCachePaginationClient(t)
	apiReader := &paginatedAPIReader{}
	handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader})
	app := newListTestApp(handlers)

	_, status := listPage[toolListTestItem](t, app, "/tools?limit=0")
	require.Equal(t, http.StatusBadRequest, status)
	require.Empty(t, apiReader.snapshotCalls())
}

func TestHandlers_ListTools_RejectsCombinedCursorFromDifferentNamespace(t *testing.T) {
	cacheClient, _ := newCachePaginationClient(t)
	apiReader := &paginatedAPIReader{}
	handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader})
	app := newListTestApp(handlers)

	first, status := listPage[toolListTestItem](t, app, "/tools?limit=1&namespace=default")
	require.Equal(t, http.StatusOK, status)
	require.NotEmpty(t, first.Metadata.Continue)

	path := pathWithContinue("/tools?limit=1&namespace=other", first.Metadata.Continue)
	_, status = listPage[toolListTestItem](t, app, path)
	require.Equal(t, http.StatusBadRequest, status)
	require.Len(t, apiReader.snapshotCalls(), 1)
}

func TestHandlers_ListTools_FiltersBeforePaginationWithoutLeakingHiddenCounts(t *testing.T) {
	authorized := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "custom-allowed", Namespace: "default"}}
	hidden := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "custom-hidden", Namespace: "default"}}
	cacheClient, cacheCalls := newCachePaginationClient(t, authorized, hidden)
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{
		Mode: ContextTokenAuthorizationModeEnforce,
	})
	require.NoError(t, err)
	handlers := NewHandlers(HandlersConfig{
		Client:                    cacheClient,
		APIReader:                 cacheClient,
		ContextTokenAuthorization: authz,
	})
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		userInfo := contextScopedTestUser(ContextTokenScopeToolsRead)
		userInfo.ContextToken.TransactionContext = map[string]any{
			"allowedTools": []string{"file_read", "custom-allowed"},
		}
		c.Locals(UserInfoContextKey, userInfo)
		return c.Next()
	})
	app.Get("/tools", handlers.ListTools)

	page, status := listPage[toolListTestItem](t, app, "/tools?limit=1")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []toolListTestItem{
		{Name: "file_read", Builtin: true},
		{Name: "custom-allowed"},
	}, page.Items)
	require.Empty(t, page.Metadata.Continue)
	require.Nil(t, page.Metadata.RemainingItemCount)
	require.Empty(t, cacheCalls.snapshot(), "filtered listing must not raw-list hidden tools")

	_, status = listPage[toolListTestItem](t, app, "/tools?limit=1&continue=opaque")
	require.Equal(t, http.StatusBadRequest, status)
}

type erroringListReader struct {
	err error
}

func (r *erroringListReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return r.err
}

func (r *erroringListReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return r.err
}

func TestHandlers_DirectListContinuationErrorsKeepKubernetesStatus(t *testing.T) {
	resources := []struct {
		name string
		path string
	}{
		{name: "tasks", path: "/tasks?limit=1&paginate=true"},
		{name: "agents", path: "/agents?limit=1"},
		{name: "skills", path: "/skills?limit=1"},
	}
	errors := []struct {
		name   string
		err    error
		status int
	}{
		{name: "expired", err: apierrors.NewResourceExpired("expired"), status: http.StatusGone},
		{name: "malformed", err: apierrors.NewBadRequest("bad continue"), status: http.StatusBadRequest},
	}

	for _, resource := range resources {
		for _, failure := range errors {
			t.Run(resource.name+"/"+failure.name, func(t *testing.T) {
				cacheClient, cacheCalls := newCachePaginationClient(t)
				handlers := NewHandlers(HandlersConfig{
					Client:    cacheClient,
					APIReader: &erroringListReader{err: failure.err},
				})
				app := newListTestApp(handlers)

				resp, err := app.Test(httptest.NewRequest(http.MethodGet, resource.path, nil))
				require.NoError(t, err)
				defer resp.Body.Close() //nolint:errcheck
				require.Equal(t, failure.status, resp.StatusCode)
				require.Empty(t, cacheCalls.snapshot())
			})
		}
	}
}

func TestHandlers_ListTools_ReturnsGoneWhenSnapshotChanges(t *testing.T) {
	cacheClient, _ := newCachePaginationClient(t)
	apiReader := &paginatedAPIReader{tools: []corev1alpha1.Tool{{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-1", Namespace: "default"},
	}}}
	handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader})
	app := newListTestApp(handlers)

	first, status := listPage[toolListTestItem](t, app, "/tools?limit=6")
	require.Equal(t, http.StatusOK, status)
	require.NotEmpty(t, first.Metadata.Continue)
	apiReader.mu.Lock()
	apiReader.tools = append(apiReader.tools, corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "custom-2", Namespace: "default"}})
	apiReader.mu.Unlock()
	_, status = listPage[toolListTestItem](t, app, pathWithContinue("/tools?limit=6", first.Metadata.Continue))
	require.Equal(t, http.StatusGone, status)
}

func TestHandlers_ListTools_AllowsRetryingTheSameCursor(t *testing.T) {
	cacheClient, _ := newCachePaginationClient(t)
	apiReader := &paginatedAPIReader{tools: []corev1alpha1.Tool{{ObjectMeta: metav1.ObjectMeta{Name: "custom", Namespace: "default"}}}}
	handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader})
	app := newListTestApp(handlers)
	first, status := listPage[toolListTestItem](t, app, "/tools?limit=1")
	require.Equal(t, http.StatusOK, status)
	path := pathWithContinue("/tools?limit=1", first.Metadata.Continue)
	second, status := listPage[toolListTestItem](t, app, path)
	require.Equal(t, http.StatusOK, status)
	retried, status := listPage[toolListTestItem](t, app, path)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, second, retried)
}

func TestHandlers_ListTasks_ContextTokenUsesAuthoritativeReaderForDependencies(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-task", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "current-agent"},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "current-agent", Namespace: "default"},
		Spec:       corev1alpha1.AgentSpec{Tools: []corev1alpha1.ToolReference{{Name: "web_search"}}},
	}
	cacheClient, _ := newCachePaginationClient(t, task)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	apiReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, agent).Build()
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	require.NoError(t, err)
	handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader, ContextTokenAuthorization: authz})
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		userInfo := contextScopedTestUser(ContextTokenScopeTaskList)
		userInfo.ContextToken.TransactionContext = map[string]any{
			"allowedAgents": []string{"current-agent"},
			"allowedTools":  []string{"recall_memory", "remember", "propose_memory", "search_transcript"},
		}
		c.Locals(UserInfoContextKey, userInfo)
		return c.Next()
	})
	app.Get("/tasks", handlers.ListTasks)

	page, status := listPage[corev1alpha1.Task](t, app, "/tasks?limit=1")
	require.Equal(t, http.StatusOK, status)
	require.Empty(t, page.Items, "authoritative Agent tools must remain part of authorization")
}

func TestHandlers_ListTasks_ContextTokenReturnsCompleteAuthorizedLogicalList(t *testing.T) {
	tasks := []corev1alpha1.Task{
		{ObjectMeta: metav1.ObjectMeta{Name: "allowed-task-1", Namespace: "default"}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "allowed-agent"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "hidden-task", Namespace: "default"}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "hidden-agent"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "allowed-task-2", Namespace: "default"}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "allowed-agent"}}},
	}
	cacheClient, _ := newCachePaginationClient(t, &tasks[0], &tasks[1], &tasks[2])
	apiReader := &paginatedAPIReader{tasks: tasks}
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	require.NoError(t, err)
	handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader, ContextTokenAuthorization: authz})
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		userInfo := contextScopedTestUser(ContextTokenScopeTaskList)
		userInfo.ContextToken.TransactionContext = map[string]any{"allowedAgents": []string{"allowed-agent"}}
		c.Locals(UserInfoContextKey, userInfo)
		return c.Next()
	})
	app.Get("/tasks", handlers.ListTasks)

	page, status := listPage[corev1alpha1.Task](t, app, "/tasks?limit=1")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{"allowed-task-1", "allowed-task-2"}, []string{page.Items[0].Name, page.Items[1].Name})
	require.Empty(t, page.Metadata.Continue)
	require.Nil(t, page.Metadata.RemainingItemCount)
	require.Equal(t, []recordedListCall{{Kind: "tasks", Namespace: "default"}}, apiReader.snapshotCalls())

	_, status = listPage[corev1alpha1.Task](t, app, "/tasks?limit=1&paginate=true&continue=opaque")
	require.Equal(t, http.StatusBadRequest, status)
}

func TestHandlers_ListAgents_ContextTokenReturnsCompleteAuthorizedLogicalList(t *testing.T) {
	agents := []corev1alpha1.Agent{
		{ObjectMeta: metav1.ObjectMeta{Name: "allowed-agent-1", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "hidden-agent", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "allowed-agent-2", Namespace: "default"}},
	}
	cacheClient, _ := newCachePaginationClient(t, &agents[0], &agents[1], &agents[2])
	apiReader := &paginatedAPIReader{agents: agents}
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	require.NoError(t, err)
	handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader, ContextTokenAuthorization: authz})
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		userInfo := contextScopedTestUser(ContextTokenScopeAgentsRead)
		userInfo.ContextToken.TransactionContext = map[string]any{
			"allowedAgents": []string{"allowed-agent-1", "allowed-agent-2"},
		}
		c.Locals(UserInfoContextKey, userInfo)
		return c.Next()
	})
	app.Get("/agents", handlers.ListAgents)

	page, status := listPage[corev1alpha1.Agent](t, app, "/agents?limit=1")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{"allowed-agent-1", "allowed-agent-2"}, []string{page.Items[0].Name, page.Items[1].Name})
	require.Empty(t, page.Metadata.Continue)
	require.Nil(t, page.Metadata.RemainingItemCount)
	require.Equal(t, []recordedListCall{{Kind: "agents", Namespace: "default"}}, apiReader.snapshotCalls())

	_, status = listPage[corev1alpha1.Agent](t, app, "/agents?limit=1&continue=opaque")
	require.Equal(t, http.StatusBadRequest, status)
}

func TestHandlers_ListTools_AllowsMoreThanCursorHistoryWindow(t *testing.T) {
	tools := make([]corev1alpha1.Tool, maxToolCursorHistorySize+2)
	objects := make([]client.Object, 0, len(tools))
	for i := range tools {
		tools[i].ObjectMeta = metav1.ObjectMeta{
			Name:      fmt.Sprintf("custom-long-%03d", i),
			Namespace: "default",
		}
		objects = append(objects, &tools[i])
	}
	cacheClient, _ := newCachePaginationClient(t, objects...)
	apiReader := &paginatedAPIReader{tools: tools}
	handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: apiReader})
	app := newListTestApp(handlers)

	path := "/tools?limit=1"
	count := 0
	seenNames := map[string]struct{}{}
	for page := 0; page < len(builtinToolsList)+len(tools)+1; page++ {
		response, status := listPage[toolListTestItem](t, app, path)
		require.Equal(t, http.StatusOK, status)
		for _, item := range response.Items {
			if _, duplicate := seenNames[item.Name]; duplicate {
				t.Fatalf("duplicate tool %q on page %d", item.Name, page)
			}
			seenNames[item.Name] = struct{}{}
		}
		count += len(response.Items)
		if response.Metadata.Continue == "" {
			break
		}
		path = pathWithContinue("/tools?limit=1", response.Metadata.Continue)
	}
	require.Equal(t, len(builtinToolsList)+len(tools), count)
}

func TestHandlers_ListTools_FilteredModeStillValidatesLimit(t *testing.T) {
	cacheClient, _ := newCachePaginationClient(t)
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	require.NoError(t, err)
	handlers := NewHandlers(HandlersConfig{Client: cacheClient, APIReader: cacheClient, ContextTokenAuthorization: authz})
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		userInfo := contextScopedTestUser(ContextTokenScopeToolsRead)
		userInfo.ContextToken.TransactionContext = map[string]any{"allowedTools": []string{"file_read"}}
		c.Locals(UserInfoContextKey, userInfo)
		return c.Next()
	})
	app.Get("/tools", handlers.ListTools)

	for _, limit := range []string{"0", "-1", "not-a-number"} {
		_, status := listPage[toolListTestItem](t, app, "/tools?limit="+url.QueryEscape(limit))
		require.Equal(t, http.StatusBadRequest, status, "limit %q", limit)
	}
}
