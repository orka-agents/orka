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
