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
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apimachineryyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	sigsyaml "sigs.k8s.io/yaml"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"

	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestShippedManifestsDecodeStrictly(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacev1alpha1.AddToScheme(scheme))
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
	require.Greater(t, checkedDocuments, 10, "expected Orka manifests under examples/ and config/samples/")
	require.Greater(t, checkedAgents, 3, "expected Agent manifests under examples/ and config/samples/")
}

func splitYAMLDocuments(path string) ([][]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return splitYAMLDocumentsFrom(file)
}

func splitYAMLDocumentsFrom(source io.Reader) ([][]byte, error) {
	reader := apimachineryyaml.NewYAMLReader(bufio.NewReader(source))
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
	gvk := groupVersion.WithKind(typeMeta.Kind)
	providerKinds := map[string][]string{
		"sandbox.workspace.orka.ai":   {"SandboxProviderConfig", "SandboxWorkspaceProfile"},
		"substrate.workspace.orka.ai": {"SubstrateProviderConfig", "SubstrateWorkspaceProfile"},
		"fake.workspace.orka.ai":      {"FakeProviderConfig", "FakeWorkspaceProfile"},
	}
	for _, kind := range providerKinds[groupVersion.Group] {
		if groupVersion.Version == "v1alpha1" && typeMeta.Kind == kind {
			return nil, false, nil
		}
	}

	if clientgoscheme.Scheme.Recognizes(gvk) ||
		(typeMeta.APIVersion == "kustomize.config.k8s.io/v1beta1" && typeMeta.Kind == "Kustomization") {
		return nil, false, nil
	}
	object, err := scheme.New(gvk)
	if err != nil {
		return nil, false, fmt.Errorf("unsupported manifest %s: %w", gvk, err)
	}
	if err := sigsyaml.UnmarshalStrict(document, object); err != nil {
		return nil, false, fmt.Errorf("strict decode %s: %w", typeMeta.Kind, err)
	}
	return object, true, nil
}

func TestStrictDecodeOrkaDocument_APIIdentity(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	for _, tc := range []struct {
		name       string
		apiVersion string
		kind       string
		checked    bool
		wantErr    bool
	}{
		{"Orka Task", "core.orka.ai/v1alpha1", "Task", true, false},
		{"misspelled Orka group", "cor.orka.ai/v1alpha1", "Task", false, true},
		{"Task in core v1", "v1", "Task", false, true},
		{"Kubernetes Namespace", "v1", "Namespace", false, false},
		{"Kubernetes Deployment", "apps/v1", "Deployment", false, false},
		{"Kustomization", "kustomize.config.k8s.io/v1beta1", "Kustomization", false, false},
		{"misspelled Kustomization kind", "kustomize.config.k8s.io/v1beta1", "Kustomizatoin", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := fmt.Sprintf("apiVersion: %s\nkind: %s\n", tc.apiVersion, tc.kind)
			object, checked, err := strictDecodeOrkaDocument(scheme, []byte(document))
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.checked, checked)
			if checked {
				require.IsType(t, &corev1alpha1.Task{}, object)
			} else {
				require.Nil(t, object)
			}
		})
	}
}

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

func TestDocumentedManifestsDecodeStrictly(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacev1alpha1.AddToScheme(scheme))
	require.NoError(t, gatewayv1alpha1.AddToScheme(scheme))

	checkedDocuments := 0
	skippedBlocks := 0
	checkMarkdown := func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		blocks, skips, err := extractYAMLCodeBlocks(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, skip := range skips {
			skippedBlocks++
			t.Logf("%s:%d: skipped strict decode: %s", path, skip.line, skip.reason)
		}
		for _, block := range blocks {
			documents, err := splitYAMLDocumentsFrom(strings.NewReader(block.body))
			if err != nil {
				if block.strict {
					return fmt.Errorf("%s:%d: YAML does not parse: %w", path, block.line, err)
				}
				continue
			}
			if block.wholeManifest && len(documents) == 0 {
				return fmt.Errorf("%s:%d: kubectl manifest is empty", path, block.line)
			}
			for index, document := range documents {
				isManifest, err := looksLikeManifest(document)
				if err != nil {
					if block.strict {
						return fmt.Errorf("%s:%d document %d: invalid YAML block: %w", path, block.line, index+1, err)
					}
					continue
				}
				if !isManifest {
					if block.wholeManifest {
						return fmt.Errorf("%s:%d document %d: kubectl manifest must include apiVersion and kind", path, block.line, index+1)
					}
					continue
				}
				object, checked, err := strictDecodeOrkaDocument(scheme, document)
				if err != nil {
					return fmt.Errorf("%s:%d document %d: %w", path, block.line, index+1, err)
				}
				if !checked || object == nil {
					continue
				}
				checkedDocuments++
			}
		}
		return nil
	}
	for _, root := range []string{
		filepath.Join("..", "..", "README.md"),
		filepath.Join("..", "..", "website", "docs"),
		filepath.Join("..", "..", "examples"),
	} {
		require.NoError(t, filepath.WalkDir(root, checkMarkdown))
	}
	t.Logf("checked %d documented manifests, skipped %d explicit exceptions", checkedDocuments, skippedBlocks)
	require.Greater(t, checkedDocuments, 40, "expected Orka manifests in documented code blocks")
}

type yamlCodeBlock struct {
	line          int
	body          string
	strict        bool
	wholeManifest bool
}

const docsStrictDecodeSkipMarker = "orka:skip-strict-decode"

type skippedDocBlock struct {
	line   int
	reason string
}

var skipDirectiveRe = regexp.MustCompile(`^<!--\s*orka:skip-strict-decode\s+(.+?)\s*-->$`)

func parseSkipDirective(line string) (reason string, isAttempt bool, valid bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.Contains(trimmed, docsStrictDecodeSkipMarker) {
		return "", false, false
	}
	if !strings.HasPrefix(trimmed, "<!--") {
		return "", false, false
	}
	m := skipDirectiveRe.FindStringSubmatch(trimmed)
	if m == nil {
		return "", true, false
	}
	reason = strings.TrimSpace(m[1])
	if reason == "" || strings.Contains(reason, "-->") || strings.Contains(reason, "<!--") {
		return "", true, false
	}
	return reason, true, true
}

func extractYAMLCodeBlocks(path string) ([]yamlCodeBlock, []skippedDocBlock, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	lines := strings.Split(string(content), "\n")
	var blocks []yamlCodeBlock
	var skips []skippedDocBlock
	for i := 0; i < len(lines); i++ {
		fence, language, ok := parseOpeningFence(lines[i])
		if !ok {
			continue
		}
		start := i
		var body []string
		i++
		for ; i < len(lines); i++ {
			closing, info, ok := parseOpeningFence(lines[i])
			if ok && info == "" && closing[0] == fence[0] && len(closing) >= len(fence) {
				break
			}
			body = append(body, lines[i])
		}
		supported := language == "yaml" || language == "yml" || language == "bash" || language == "sh" || language == "shell" || language == "console"
		if supported && start > 0 {
			reason, isAttempt, valid := parseSkipDirective(lines[start-1])
			if valid {
				skips = append(skips, skippedDocBlock{line: start + 1, reason: reason})
				continue
			}
			if isAttempt {
				return nil, nil, fmt.Errorf("line %d: malformed orka:skip-strict-decode directive (expected <!-- orka:skip-strict-decode reason --> with a nonblank reason)", start)
			}
		}
		switch language {
		case "yaml", "yml":
			blocks = append(blocks, yamlCodeBlock{line: start + 2, body: strings.Join(body, "\n"), strict: true})
		case "bash", "sh", "shell", "console":
			heredocs, err := extractShellHeredocs(start+2, body)
			if err != nil {
				return nil, nil, err
			}
			blocks = append(blocks, heredocs...)
		}
	}
	return blocks, skips, nil
}

var heredocOpener = regexp.MustCompile(`<<(-?)\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)

var kubectlManifestHeredoc = regexp.MustCompile(`^\s*(?:\$\s+)?kubectl\b.*\b(?:apply|create)\b.*(?:-f\s+|--filename(?:=|\s+))-(?:\s|$)`)

func extractShellHeredocs(baseLine int, lines []string) ([]yamlCodeBlock, error) {
	var blocks []yamlCodeBlock
	for i := 0; i < len(lines); i++ {
		match := heredocOpener.FindStringSubmatch(lines[i])
		if match == nil {
			continue
		}
		stripTabs := match[1] == "-"
		delimiter := match[2]
		wholeManifest := kubectlManifestHeredoc.MatchString(lines[i])
		start := i
		var body []string
		i++
		for ; i < len(lines); i++ {
			line := lines[i]
			if stripTabs {
				line = strings.TrimLeft(line, "\t")
			}
			if line == delimiter {
				break
			}
			body = append(body, line)
		}
		if i == len(lines) {
			return nil, fmt.Errorf("line %d: unterminated heredoc %q", baseLine+start, delimiter)
		}
		blocks = append(blocks, yamlCodeBlock{
			line: baseLine + start + 1, body: strings.Join(body, "\n"),
			strict: wholeManifest, wholeManifest: wholeManifest,
		})
	}
	return blocks, nil
}

func parseOpeningFence(line string) (string, string, bool) {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return "", "", false
	}
	run := 0
	for run < len(trimmed) && trimmed[run] == trimmed[0] {
		run++
	}
	if run < 3 {
		return "", "", false
	}
	info := strings.TrimSpace(trimmed[run:])
	language, _, _ := strings.Cut(info, " ")
	return trimmed[:run], strings.ToLower(language), true
}

func TestExtractYAMLCodeBlocks(t *testing.T) {
	for _, fence := range []string{"```", "~~~"} {
		for _, command := range []string{"kubectl apply -f -", "kubectl -n orka-system create --filename=-"} {
			t.Run(fence+"/"+command, func(t *testing.T) {
				const manifest = "apiVersion: core.orka.ai/v1alpha1\nkind: Task"
				content := strings.Join([]string{
					"# Example", "", fence + "yaml title=task", manifest, fence + fence[:1], "",
					fence + "bash", command + " <<'EOF'", manifest, "EOF",
					"cat <<'TEXT'", "[arbitrary text", "TEXT", fence,
				}, "\n")
				path := filepath.Join(t.TempDir(), "example.md")
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

				blocks, skips, err := extractYAMLCodeBlocks(path)
				require.NoError(t, err)
				require.Empty(t, skips)
				require.Equal(t, []yamlCodeBlock{
					{line: 4, body: manifest, strict: true},
					{line: 10, body: manifest, strict: true, wholeManifest: true},
					{line: 14, body: "[arbitrary text"},
				}, blocks)
			})
		}
	}
}

func TestExtractYAMLCodeBlocks_SkipValid(t *testing.T) {
	const manifest = "apiVersion: core.orka.ai/v1alpha1\nkind: Task"
	for _, fence := range []string{"```", "~~~"} {
		t.Run("yaml/"+fence, func(t *testing.T) {
			content := strings.Join([]string{
				"# Example", "",
				"<!-- orka:skip-strict-decode Intentionally invalid manifest used to explain a validation error. -->",
				fence + "yaml", "kind: [Task", fence,
				"", fence + "yaml", manifest, fence,
			}, "\n")
			path := filepath.Join(t.TempDir(), "example.md")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

			blocks, skips, err := extractYAMLCodeBlocks(path)
			require.NoError(t, err)
			require.Equal(t, []skippedDocBlock{{line: 4, reason: "Intentionally invalid manifest used to explain a validation error."}}, skips)
			require.Equal(t, []yamlCodeBlock{{line: 9, body: manifest, strict: true}}, blocks)
		})
		t.Run("shell-heredoc/"+fence, func(t *testing.T) {
			content := strings.Join([]string{
				"<!-- orka:skip-strict-decode Demonstrates a rejected wristband. -->",
				fence + "bash", "kubectl apply -f - <<'EOF'", manifest, "EOF", fence,
				"", fence + "bash", "kubectl apply -f - <<'EOF'", manifest, "EOF", fence,
			}, "\n")
			path := filepath.Join(t.TempDir(), "example.md")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

			blocks, skips, err := extractYAMLCodeBlocks(path)
			require.NoError(t, err)
			require.Equal(t, []skippedDocBlock{{line: 2, reason: "Demonstrates a rejected wristband."}}, skips)
			require.Equal(t, []yamlCodeBlock{{line: 11, body: manifest, strict: true, wholeManifest: true}}, blocks)
		})
		t.Run("whitespace/"+fence, func(t *testing.T) {
			content := strings.Join([]string{
				"   <!-- orka:skip-strict-decode    Trimmed reason with surrounding whitespace.   -->   ",
				fence + "yaml", manifest, fence,
			}, "\n")
			path := filepath.Join(t.TempDir(), "example.md")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

			blocks, skips, err := extractYAMLCodeBlocks(path)
			require.NoError(t, err)
			require.Equal(t, []skippedDocBlock{{line: 2, reason: "Trimmed reason with surrounding whitespace."}}, skips)
			require.Empty(t, blocks)
		})
	}
}

func TestExtractYAMLCodeBlocks_SkipBlankReason(t *testing.T) {
	for _, directive := range []string{
		"<!-- orka:skip-strict-decode -->",
		"<!-- orka:skip-strict-decode   -->",
		"<!--  orka:skip-strict-decode\t-->",
	} {
		t.Run(directive, func(t *testing.T) {
			content := strings.Join([]string{
				directive, "```yaml", "apiVersion: core.orka.ai/v1alpha1\nkind: Task", "```",
			}, "\n")
			path := filepath.Join(t.TempDir(), "example.md")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

			_, _, err := extractYAMLCodeBlocks(path)
			require.Error(t, err)
			require.Contains(t, err.Error(), "line 1: malformed orka:skip-strict-decode directive")
		})
	}
}

func TestExtractYAMLCodeBlocks_SkipMalformed(t *testing.T) {
	for _, directive := range []string{
		"<!-- orka:skip-strict-decode reason",
		"<!-- orka:skip-strict-decode reason --> trailing prose",
		"<!-- orka:skip-strict-decode reason --",
		"<!-- orka:skip-strict-decode --> <!-- unrelated -->",
		"<!-- orka:skip-strict-decode reason --> <!-- unrelated -->",
	} {
		t.Run(directive, func(t *testing.T) {
			content := strings.Join([]string{
				directive, "```yaml", "apiVersion: core.orka.ai/v1alpha1\nkind: Task", "```",
			}, "\n")
			path := filepath.Join(t.TempDir(), "example.md")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

			_, _, err := extractYAMLCodeBlocks(path)
			require.Error(t, err)
			require.Contains(t, err.Error(), "line 1: malformed orka:skip-strict-decode directive")
		})
	}
}

func TestExtractYAMLCodeBlocks_SkipIgnored(t *testing.T) {
	const manifest = "apiVersion: core.orka.ai/v1alpha1\nkind: Task"
	content := strings.Join([]string{
		"# Example", "",
		"<!-- just an ordinary comment -->",
		"```yaml", manifest, "```", "",
		"See <!-- orka:skip-strict-decode quoted reason --> for details.",
		"```yaml", manifest, "```", "",
		"<!-- orka:skip-strict-decode Stale directive far from any fence. -->",
		"", "",
		"```yaml", manifest, "```",
	}, "\n")
	path := filepath.Join(t.TempDir(), "example.md")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	blocks, skips, err := extractYAMLCodeBlocks(path)
	require.NoError(t, err)
	require.Empty(t, skips)
	require.Len(t, blocks, 3)
}

func TestExtractYAMLCodeBlocks_SkipAppliesOnlyToNextFence(t *testing.T) {
	const manifest = "apiVersion: core.orka.ai/v1alpha1\nkind: Task"
	content := strings.Join([]string{
		"<!-- orka:skip-strict-decode Skips only the first fence. -->",
		"```yaml", "kind: [Task", "```", "",
		"```yaml", manifest, "```",
	}, "\n")
	path := filepath.Join(t.TempDir(), "example.md")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	blocks, skips, err := extractYAMLCodeBlocks(path)
	require.NoError(t, err)
	require.Equal(t, []skippedDocBlock{{line: 2, reason: "Skips only the first fence."}}, skips)
	require.Equal(t, []yamlCodeBlock{{line: 7, body: manifest, strict: true}}, blocks)
}

func TestExtractYAMLCodeBlocks_HeredocTerminators(t *testing.T) {
	const manifest = "apiVersion: core.orka.ai/v1alpha1\nkind: Task"
	for _, tc := range []struct {
		name       string
		opener     string
		body       string
		terminator string
		wantErr    bool
	}{
		{"plain", "<<EOF", manifest, "EOF", false},
		{"missing terminator", "<<EOF", manifest, "", true},
		{"space-indented terminator", "<<EOF", manifest, " EOF", true},
		{"tab-indented terminator", "<<EOF", manifest, "\tEOF", true},
		{"trailing whitespace", "<<EOF", manifest, "EOF ", true},
		{"strip leading tabs", "<<-'EOF'", "\tapiVersion: core.orka.ai/v1alpha1\n\t\tkind: Task", "\t\tEOF", false},
		{"spaces are not stripped", "<<-EOF", manifest, "\t EOF", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "example.md")
			content := strings.Join([]string{
				"```bash", "kubectl apply -f - " + tc.opener, tc.body, tc.terminator, "```",
			}, "\n")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

			blocks, skips, err := extractYAMLCodeBlocks(path)
			if tc.wantErr {
				require.EqualError(t, err, "line 2: unterminated heredoc \"EOF\"")
				return
			}
			require.NoError(t, err)
			require.Empty(t, skips)
			require.Equal(t, []yamlCodeBlock{{line: 3, body: manifest, strict: true, wholeManifest: true}}, blocks)
		})
	}
}

func looksLikeManifest(document []byte) (bool, error) {
	var probe any
	if err := sigsyaml.Unmarshal(document, &probe); err != nil {
		return false, err
	}
	mapping, isMapping := probe.(map[string]any)
	if !isMapping {
		return false, nil
	}
	_, hasAPIVersion := mapping["apiVersion"]
	_, hasKind := mapping["kind"]
	if !hasAPIVersion && !hasKind {
		return false, nil
	}

	var typeMeta metav1.TypeMeta
	if err := sigsyaml.Unmarshal(document, &typeMeta); err != nil {
		return false, err
	}
	if typeMeta.APIVersion == "" || typeMeta.Kind == "" {
		return false, errors.New("manifest must include non-empty apiVersion and kind")
	}
	return true, nil
}

func TestLooksLikeManifest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		document string
		manifest bool
		wantErr  bool
	}{
		{"manifest", "apiVersion: core.orka.ai/v1alpha1\nkind: Task", true, false},
		{"fragment", "spec:\n  agentRef: coder", false, false},
		{"list", "- kind: Task", false, false},
		{"scalar", "Task", false, false},
		{"missing apiVersion", "kind: Task", false, true},
		{"missing kind", "apiVersion: core.orka.ai/v1alpha1", false, true},
		{"misspelled apiVersion", "apiVerison: core.orka.ai/v1alpha1\nkind: Task", false, true},
		{"misspelled kind", "apiVersion: core.orka.ai/v1alpha1\nknd: Task", false, true},
		{"empty metadata", "apiVersion: ''\nkind: ''", false, true},
		{"invalid YAML", "kind: [Task", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest, err := looksLikeManifest([]byte(tc.document))
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.manifest, manifest)
		})
	}
}
