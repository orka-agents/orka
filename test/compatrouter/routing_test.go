package compatrouter

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestDeployedCompatibilityRouting(t *testing.T) {
	f := newLiveEnvironment(t)
	for _, namespace := range []string{"team-a", "team-b"} {
		f.waitForTasks(t, namespace, 1)
		// Only this namespace's controller is allowed to launch these Jobs.
		for _, target := range []string{"team-a", "team-b"} {
			review, err := f.clientset.AuthorizationV1().
				SubjectAccessReviews().
				Create(t.Context(), &authorizationv1.SubjectAccessReview{
					Spec: authorizationv1.SubjectAccessReviewSpec{
						User: "system:serviceaccount:" + namespace + ":controller",
						ResourceAttributes: &authorizationv1.ResourceAttributes{
							Namespace: target,
							Group:     "batch",
							Resource:  "jobs",
							Verb:      "create",
						},
					},
				}, metav1.CreateOptions{})
			require.NoError(t, err)
			require.Equal(t, namespace == target, review.Status.Allowed)
		}
	}

	t.Run("concurrent namespace and protocol isolation", func(t *testing.T) {
		for _, namespace := range []string{"team-a", "team-b"} {
			for _, path := range []string{"/openai/v1/chat/completions", "/anthropic/v1/messages"} {
				for _, stream := range []bool{false, true} {
					t.Run(
						fmt.Sprintf("%s%s/stream=%t", namespace, path, stream),
						func(t *testing.T) {
							t.Parallel()
							other := "team-a"
							if namespace == "team-a" {
								other = "team-b"
							}
							for _, prompt := range []string{"create", "read", "agents"} {
								status, body := f.request(
									t,
									namespace,
									"editor",
									path,
									prompt,
									stream,
									false,
								)
								require.Equal(t, http.StatusOK, status, body)
								text := responseText(t, body, stream)
								require.Contains(t, text, "OUTPUT:"+namespace)
								require.NotContains(t, text, "OUTPUT:"+other)
								require.NotContains(t, text, "RESULT:"+other)
								if prompt == "agents" {
									require.Contains(t, text, namespace+"-agent")
									require.NotContains(t, text, other+"-agent")
								} else {
									require.Contains(
										t,
										text,
										"RESULT:"+namespace,
										"the worker result must return through the selected installation",
									)
								}
							}
							status, body := f.request(
								t,
								namespace,
								"editor",
								path,
								"create",
								stream,
								true,
							)
							require.Equal(t, http.StatusOK, status, body)
							require.Contains(t, responseText(t, body, stream), "tools disabled")
							for _, prompt := range []string{"create " + other, "read " + other, "agents " + other} {
								status, body := f.request(
									t,
									namespace,
									"editor",
									path,
									prompt,
									stream,
									false,
								)
								require.Equal(t, http.StatusOK, status, body)
								text := responseText(t, body, stream)
								require.Contains(t, text, `"success":false`)
								require.NotContains(t, text, "RESULT:"+other)
								require.NotContains(t, text, other+"-agent")
							}
						},
					)
				}
			}
		}
	})

	for _, namespace := range []string{"team-a", "team-b"} {
		for _, path := range []string{"/openai/v1/models", "/anthropic/v1/models"} {
			status, body := f.request(t, namespace, "editor", path, "", false, false)
			require.Equal(t, http.StatusOK, status, body)
			require.Contains(t, body, "shared/"+namespace+"-catalog")
			status, _ = f.request(t, namespace, "denied", path, "", false, false)
			require.Equal(t, http.StatusForbidden, status)
		}
		for _, path := range []string{"/openai/v1/chat/completions", "/anthropic/v1/messages"} {
			status, _ := f.request(t, namespace, "denied", path, "create", false, false)
			require.Equal(t, http.StatusForbidden, status)
			status, body := f.request(t, namespace, "chat-only", path, "create", false, false)
			require.Equal(t, http.StatusOK, status, body)
			require.Contains(t, responseText(t, body, false), `"success":false`)
		}
		// One seed Task plus one per API/stream combination. Disabled tools,
		// cross-namespace arguments and insufficient RBAC cannot add Tasks.
		f.verifyExecutions(t, namespace, 5)
	}
}

func (f *liveEnvironment) request(
	t *testing.T,
	namespace, role, path, prompt string,
	stream, disabled bool,
) (int, string) {
	t.Helper()
	method := http.MethodPost
	if strings.HasSuffix(path, "/models") {
		method = http.MethodGet
	}
	// The body is identical for both namespaces. Only the credential changes.
	data, err := json.Marshal(map[string]any{
		"model":      "shared/model",
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": 1024,
		"stream":     stream,
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(
		t.Context(),
		method,
		f.url+path,
		strings.NewReader(string(data)),
	)
	require.NoError(t, err)
	if strings.HasPrefix(path, "/anthropic/") {
		req.Header.Set("x-api-key", f.tokens[namespace+"/"+role])
		req.Header.Set("Anthropic-Version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+f.tokens[namespace+"/"+role])
	}
	req.Header.Set("Content-Type", "application/json")
	if disabled {
		req.Header.Set("X-Orka-Tools", "disabled")
	}
	response, err := (&http.Client{Timeout: 4 * time.Minute}).Do(req)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	// Do not print a leaked credential even when a response assertion fails.
	for _, token := range f.tokens {
		require.False(
			t,
			strings.Contains(string(body), token),
			"response contained a caller credential",
		)
	}
	require.NotContains(t, string(body), "fixture-team-", "Provider credential reached the caller")
	if stream && response.StatusCode == http.StatusOK {
		require.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
		if strings.HasPrefix(path, "/anthropic/") {
			require.Contains(t, string(body), "event: message_stop")
		} else {
			require.Contains(t, string(body), "data: [DONE]")
		}
	}
	return response.StatusCode, string(body)
}

func responseText(t *testing.T, body string, stream bool) string {
	t.Helper()
	var text strings.Builder
	for line := range strings.SplitSeq(body, "\n") {
		if stream {
			if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
				continue
			}
			line = strings.TrimPrefix(line, "data: ")
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event struct {
			Choices []struct {
				Message struct{ Content string }
				Delta   struct{ Content string }
			}
			Content []struct{ Text string }
			Delta   struct{ Text string }
		}
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		for _, choice := range event.Choices {
			text.WriteString(choice.Message.Content)
			text.WriteString(choice.Delta.Content)
		}
		for _, content := range event.Content {
			text.WriteString(content.Text)
		}
		text.WriteString(event.Delta.Text)
	}
	return text.String()
}

func (f *liveEnvironment) waitForTasks(
	t *testing.T,
	namespace string,
	count int,
) []corev1alpha1.Task {
	t.Helper()
	var tasks corev1alpha1.TaskList
	require.Eventually(t, func() bool {
		if f.kube.List(t.Context(), &tasks, client.InNamespace(namespace)) != nil ||
			len(tasks.Items) != count {
			return false
		}
		for _, task := range tasks.Items {
			if task.Status.Phase != corev1alpha1.TaskPhaseSucceeded ||
				task.Status.ResultRef == nil ||
				!task.Status.ResultRef.Available {
				return false
			}
		}
		return true
	}, 3*time.Minute, time.Second, "%s must complete exactly %d Tasks with persisted results", namespace, count)
	return tasks.Items
}

func (f *liveEnvironment) verifyExecutions(t *testing.T, namespace string, count int) {
	t.Helper()
	for _, task := range f.waitForTasks(t, namespace, count) {
		require.NotEmpty(t, task.Status.JobName)
		var job batchv1.Job
		require.NoError(
			t,
			f.kube.Get(
				t.Context(),
				client.ObjectKey{Namespace: namespace, Name: task.Status.JobName},
				&job,
			),
		)
		require.Equal(t, task.Status.JobUID, string(job.UID))
		require.True(t, metav1.IsControlledBy(&job, &task))
		require.EqualValues(t, 1, job.Status.Succeeded)
		require.Equal(t, "orka-container-worker", job.Spec.Template.Spec.ServiceAccountName)
		var pods corev1.PodList
		require.NoError(
			t,
			f.kube.List(
				t.Context(),
				&pods,
				client.InNamespace(namespace),
				client.MatchingLabels{"job-name": job.Name},
			),
		)
		require.Len(t, pods.Items, 1)
		pod := pods.Items[0]
		require.Equal(t, corev1.PodSucceeded, pod.Status.Phase)
		require.True(t, metav1.IsControlledBy(&pod, &job))
		var callback, resultEndpoint string
		for _, env := range pod.Spec.Containers[0].Env {
			switch env.Name {
			case "ORKA_CONTROLLER_URL":
				callback = env.Value
			case "ORKA_RESULT_ENDPOINT":
				resultEndpoint = env.Value
			}
		}
		require.Equal(t, "http://api."+namespace+".svc:8080", callback)
		require.Equal(t, callback+"/internal/v1/results/"+namespace+"/"+task.Name, resultEndpoint)
	}
	t.Logf(
		"%s/controller completed %d Tasks with namespace-bound worker callbacks and persisted results",
		namespace,
		count,
	)
}
