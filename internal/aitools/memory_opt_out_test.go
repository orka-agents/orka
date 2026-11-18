package aitools

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestMemoryAutoEnableOptOutMatchesToolAuthorization(t *testing.T) {
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
		Type: corev1alpha1.TaskTypeAI,
		AI:   &corev1alpha1.AISpec{Tools: []string{"file_read"}},
		Env:  []corev1.EnvVar{{Name: "ORKA_MEMORY_TOOLS_AUTO_ENABLE", Value: " false "}},
	}}
	if got := Resolve(task, nil); !slices.Equal(got, []string{"file_read"}) {
		t.Fatalf("opt-out selected unexpected tools: %v", got)
	}
	if deprecated := DeprecatedMemoryToolNames(task, nil); len(deprecated) != 0 {
		t.Fatalf("opt-out retained deprecation warnings for unselected tools: %v", deprecated)
	}
	for _, name := range MemoryToolNames() {
		if IsImplicitTool(task, nil, name) {
			t.Fatalf("opted-out tool %q retained implicit authorization", name)
		}
	}
	task.Spec.AI.Tools = []string{"remember"}
	if got := Resolve(task, nil); !slices.Equal(got, []string{"remember"}) {
		t.Fatalf("explicit memory selection was lost or widened: %v", got)
	}
	task.Spec.Env = append(task.Spec.Env, corev1.EnvVar{Name: "ORKA_MEMORY_TOOLS_AUTO_ENABLE", Value: "true"})
	if got := Resolve(task, nil); !slices.Contains(got, "recall_memory") {
		t.Fatal("last environment value must retain ordinary automatic memory behavior")
	}
}
