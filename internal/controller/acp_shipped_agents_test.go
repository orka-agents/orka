package controller

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apimachineryyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// TestShippedCodexAgentsHaveAnEnforceableToolPolicy dispatches every shipped
// built-in codex Agent's default tool policy through the same normalization
// and native-policy check a read-intent Task meets, so an example cannot ship
// a policy that fails every Task at dispatch.
func TestShippedCodexAgentsHaveAnEnforceableToolPolicy(t *testing.T) {
	checked := 0
	err := filepath.WalkDir(filepath.Join("..", "..", "examples"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml")) {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		reader := apimachineryyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
		for {
			document, err := reader.Read()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			var agent corev1alpha1.Agent
			if yaml.Unmarshal(document, &agent) != nil || agent.Kind != "Agent" || agent.Spec.Runtime == nil ||
				agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeCodex || agent.Spec.Runtime.RuntimeRef != nil {
				continue
			}
			task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead}}}
			intent := effectiveACPWorkspaceIntent(task)
			allowed, disallowed, allowBash := normalizeACPRuntimeToolPolicy(string(agent.Spec.Runtime.Type), intent,
				effectiveACPAllowedTools(task, &agent), nil, effectiveACPAllowBash(task, &agent))
			if err := validateACPProviderNativePolicy(string(agent.Spec.Runtime.Type), intent, allowed, disallowed, allowBash); err != nil {
				t.Errorf("%s Agent %s: %v", path, agent.Name, err)
			}
			checked++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no shipped codex Agents were checked")
	}
}
