package admission

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apimachineryyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	sigsyaml "sigs.k8s.io/yaml"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	fakeworkspacev1alpha1 "github.com/orka-agents/orka/api/fake.workspace/v1alpha1"
	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"

	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestShippedManifestsDecodeStrictly guards the YAML that users copy first.
// Every Orka-group document under examples/ and config/samples/ must
// strict-decode into its typed API object (catching unknown or misspelled
// fields that the API server would reject), and every built-in runtime Agent
// must pass the same admission contract the live webhook enforces, including
// the harness-v2 "spec.model.name is required" rule.
func TestShippedManifestsDecodeStrictly(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacev1alpha1.AddToScheme(scheme))
	require.NoError(t, acpworkspacev1alpha1.AddToScheme(scheme))
	require.NoError(t, fakeworkspacev1alpha1.AddToScheme(scheme))
	require.NoError(t, gatewayv1alpha1.AddToScheme(scheme))

	admissionScheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(admissionScheme))
	require.NoError(t, corev1alpha1.AddToScheme(admissionScheme))

	roots := []string{
		filepath.Join("..", "..", "examples"),
		filepath.Join("..", "..", "config", "samples"),
	}
	checkedDocuments := 0
	checkedAgents := 0
	for _, root := range roots {
		require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml")) {
				return nil
			}
			documents, err := splitYAMLDocuments(path)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			for index, document := range documents {
				object, checked, err := strictDecodeOrkaDocument(scheme, document)
				if err != nil {
					return fmt.Errorf("%s document %d: %w", path, index+1, err)
				}
				if !checked {
					continue
				}
				checkedDocuments++
				if agent, ok := object.(*corev1alpha1.Agent); ok {
					checkedAgents++
					if err := validateExampleAgentContract(t, admissionScheme, agent); err != nil {
						return fmt.Errorf("%s document %d: %w", path, index+1, err)
					}
				}
			}
			return nil
		}))
	}
	// Guard the guard: if the walk finds nothing, the roots moved and the
	// test is validating air.
	require.Greater(t, checkedDocuments, 10, "expected Orka manifests under examples/ and config/samples/")
	require.Greater(t, checkedAgents, 3, "expected Agent manifests under examples/ and config/samples/")
}

func splitYAMLDocuments(path string) ([][]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	reader := apimachineryyaml.NewYAMLReader(bufio.NewReader(file))
	var documents [][]byte
	for {
		document, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return documents, nil
		}
		if err != nil {
			return nil, err
		}
		if len(strings.TrimSpace(string(document))) == 0 {
			continue
		}
		documents = append(documents, document)
	}
}

// strictDecodeOrkaDocument decodes one YAML document into its registered Orka
// typed object with unknown fields rejected. Non-Orka documents (core v1,
// kustomize, third-party kinds) are skipped: this test owns Orka's API
// surface, not Kubernetes'.
func strictDecodeOrkaDocument(scheme *runtime.Scheme, document []byte) (runtime.Object, bool, error) {
	var typeMeta metav1.TypeMeta
	if err := sigsyaml.Unmarshal(document, &typeMeta); err != nil {
		return nil, false, fmt.Errorf("read apiVersion/kind: %w", err)
	}
	if typeMeta.APIVersion == "" || typeMeta.Kind == "" {
		return nil, false, nil
	}
	groupVersion, err := schema.ParseGroupVersion(typeMeta.APIVersion)
	if err != nil {
		return nil, false, fmt.Errorf("parse apiVersion %q: %w", typeMeta.APIVersion, err)
	}
	if !strings.HasSuffix(groupVersion.Group, "orka.ai") {
		return nil, false, nil
	}
	object, err := scheme.New(groupVersion.WithKind(typeMeta.Kind))
	if err != nil {
		return nil, false, fmt.Errorf("kind %s is not registered in the Orka scheme: %w", typeMeta.Kind, err)
	}
	if err := sigsyaml.UnmarshalStrict(document, object); err != nil {
		return nil, false, fmt.Errorf("strict decode %s: %w", typeMeta.Kind, err)
	}
	return object, true, nil
}

// validateExampleAgentContract runs a shipped Agent through the real
// AgentContractValidator under a harness-v2 namespace, mirroring the mutating
// contract defaulter that runs first in a live cluster. This is what catches
// an example Agent that would deploy but fail at dispatch, such as a built-in
// runtime Agent without spec.model.name.
func validateExampleAgentContract(t *testing.T, scheme *runtime.Scheme, agent *corev1alpha1.Agent) error {
	t.Helper()
	object := agent.DeepCopy()
	if object.Namespace == "" {
		object.Namespace = "default"
	}
	if err := executionmode.DefaultBuiltInAgentContract(object, executionmode.HarnessV2); err != nil {
		return fmt.Errorf("default built-in contract: %w", err)
	}
	namespace := &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   object.Namespace,
			Labels: map[string]string{executionmode.NamespaceLabel: string(executionmode.HarnessV2)},
		},
	}
	validator := &AgentContractValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		reader:  fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(namespace).Build(),
	}
	raw, err := json.Marshal(object)
	if err != nil {
		return fmt.Errorf("marshal Agent: %w", err)
	}
	response := validator.Handle(context.Background(), ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: object.Namespace,
		Object:    runtime.RawExtension{Raw: raw},
	}})
	if !response.Allowed {
		return fmt.Errorf("Agent %q would be denied by admission: %s", object.Name, response.Result.Message)
	}
	return nil
}
