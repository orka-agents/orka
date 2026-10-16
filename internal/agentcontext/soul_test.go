package agentcontext

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestSoulSourceValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		source *corev1alpha1.SoulSource
		valid  bool
	}{
		{"absent", nil, true},
		{"inline", &corev1alpha1.SoulSource{Inline: "Be direct."}, true},
		{"empty", &corev1alpha1.SoulSource{}, false},
		{"whitespace", &corev1alpha1.SoulSource{Inline: " \n"}, false},
		{"nul", &corev1alpha1.SoulSource{Inline: "x\x00y"}, false},
		{"invalid utf8", &corev1alpha1.SoulSource{Inline: string([]byte{0xff})}, false},
		{"byte bound", &corev1alpha1.SoulSource{Inline: strings.Repeat("語", MaxSoulBytes/3+1)}, false},
		{"wrong digest", &corev1alpha1.SoulSource{Inline: "one", Digest: Digest("two")}, false},
		{"both sources", &corev1alpha1.SoulSource{Inline: "one", ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "soul", Key: "SOUL.md"}}, false},
		{"unpinned map", &corev1alpha1.SoulSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "soul", Key: "SOUL.md"}}, false},
		{"pinned map", &corev1alpha1.SoulSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "soul", Key: "SOUL.md"}, Digest: Digest("one")}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateSource(test.source)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
		})
	}
}

func TestSoulResolutionAndComposition(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	text := "  Be direct.\nLiteral $(NAME), $$, {env:NAME}, @mention.\n"
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "soul-v1"}, Data: map[string]string{"SOUL.md": text}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team"}, Spec: corev1alpha1.AgentSpec{Soul: &corev1alpha1.SoulSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: cm.Name, Key: "SOUL.md"}, Digest: Digest(text)}}}
	soul, err := ResolveSoul(context.Background(), c, agent)
	if err != nil {
		t.Fatal(err)
	}
	if soul.Text != text || soul.Digest != Digest(text) {
		t.Fatal("source bytes changed")
	}
	base := " \nRole instructions.\n"
	if Compose(base, nil) != base {
		t.Fatal("no-soul prompt changed")
	}
	composed := Compose(base, soul)
	if !strings.HasPrefix(composed, base) || strings.Count(composed, text) != 1 {
		t.Fatal("persona not composed exactly once")
	}
	cm.Data["SOUL.md"] = "replacement"
	if err := c.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSoul(context.Background(), c, agent); !IsInvalidSource(err) {
		t.Fatalf("replacement was not rejected: %v", err)
	}
	agent.Namespace = "another-team"
	if _, err := ResolveSoul(context.Background(), c, agent); !IsInvalidSource(err) {
		t.Fatal("source escaped the Agent namespace")
	}
}

func TestSessionSoulDigestIgnoresTaskGenerationOnly(t *testing.T) {
	b := &corev1alpha1.TaskSoulBinding{TaskGeneration: 1, AgentUID: "agent", AgentGeneration: 1, SoulDigest: Digest("soul"), PromptDigest: Digest("prompt")}
	before := SessionDigest(b)
	b.TaskGeneration = 2
	if SessionDigest(b) != before {
		t.Fatal("Task generation split a Session")
	}
	b.AgentGeneration++
	if SessionDigest(b) == before {
		t.Fatal("Agent revision did not split a Session")
	}
}

func TestTaskInputsDigestIgnoresOnlyControlFields(t *testing.T) {
	spec := corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "iterate"}
	base, err := TaskInputsDigest(&spec)
	if err != nil {
		t.Fatal(err)
	}
	paused := spec
	paused.Suspend = new(true)
	if got, err := TaskInputsDigest(&paused); err != nil || got != base {
		t.Fatalf("suspend changed the inputs digest: %v", err)
	}
	edited := spec
	edited.Prompt = "a different task"
	if got, err := TaskInputsDigest(&edited); err != nil || got == base {
		t.Fatalf("a prompt edit kept the inputs digest: %v", err)
	}
	// An explicitly empty native tool policy is a different input than none.
	none, empty := spec, spec
	none.AI = &corev1alpha1.AISpec{}
	empty.AI = &corev1alpha1.AISpec{Tools: []string{}}
	noneDigest, err := TaskInputsDigest(&none)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := TaskInputsDigest(&empty); err != nil || got == noneDigest {
		t.Fatalf("an explicitly empty tool policy matched an omitted one: %v", err)
	}
	// An explicitly empty node selector or toleration list clears the Agent's
	// placement, while an omitted one inherits it.
	omitted := spec
	omitted.Execution = &corev1alpha1.ExecutionSpec{}
	omittedDigest, err := TaskInputsDigest(&omitted)
	if err != nil {
		t.Fatal(err)
	}
	for name, execution := range map[string]*corev1alpha1.ExecutionSpec{
		"node selector": {NodeSelector: map[string]string{}},
		"tolerations":   {Tolerations: []corev1.Toleration{}},
	} {
		cleared := spec
		cleared.Execution = execution
		if got, err := TaskInputsDigest(&cleared); err != nil || got == omittedDigest {
			t.Fatalf("an explicitly empty %s matched an omitted one: %v", name, err)
		}
	}
	// The digest is deterministic, including map order.
	selector := spec
	selector.Execution = &corev1alpha1.ExecutionSpec{NodeSelector: map[string]string{"a": "1", "b": "2", "c": "3"}}
	first, err := TaskInputsDigest(&selector)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if got, err := TaskInputsDigest(&selector); err != nil || got != first {
			t.Fatalf("the inputs digest is not deterministic: %v", err)
		}
	}
}

func TestSoulBindingMatches(t *testing.T) {
	recorded := corev1alpha1.TaskSoulBinding{TaskGeneration: 1, AgentUID: "agent-uid", AgentGeneration: 1,
		SoulDigest: Digest("soul"), PromptDigest: Digest("prompt"), InputsDigest: Digest("inputs")}
	advanced := recorded
	advanced.TaskGeneration = 2
	if !SoulBindingMatches(recorded, advanced) {
		t.Fatal("a control-field generation bump invalidated the binding")
	}
	for name, mutate := range map[string]func(*corev1alpha1.TaskSoulBinding){
		"inputs":           func(b *corev1alpha1.TaskSoulBinding) { b.InputsDigest = Digest("other") },
		"agent generation": func(b *corev1alpha1.TaskSoulBinding) { b.AgentGeneration = 2 },
		"prompt":           func(b *corev1alpha1.TaskSoulBinding) { b.PromptDigest = Digest("other") },
	} {
		changed := advanced
		mutate(&changed)
		if SoulBindingMatches(recorded, changed) {
			t.Fatalf("a changed %s kept the binding", name)
		}
	}
	legacy := recorded
	legacy.InputsDigest = ""
	if !SoulBindingMatches(legacy, recorded) || SoulBindingMatches(legacy, advanced) {
		t.Fatal("a binding without an inputs digest must keep the exact generation check")
	}
	if SessionDigest(&recorded) != SessionDigest(&corev1alpha1.TaskSoulBinding{AgentUID: "agent-uid", AgentGeneration: 1,
		SoulDigest: Digest("soul"), PromptDigest: Digest("prompt"), InputsDigest: Digest("another task")}) {
		t.Fatal("different Task inputs split the Session identity")
	}
}
