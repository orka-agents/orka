package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	outputTable  = "table"
	outputJSON   = "json"
	outputYAML   = "yaml"
	cliQueryTrue = "true"
)

func addOutputFlag(cmd *cobra.Command, defaultValue string) {
	cmd.Flags().StringP("output", "o", defaultValue, "Output format: table, json, yaml")
}

func outputFormat(cmd *cobra.Command) (string, error) {
	format, _ := cmd.Flags().GetString("output")
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		format = outputTable
	}
	switch format {
	case outputTable, outputJSON, outputYAML:
		return format, nil
	default:
		return "", fmt.Errorf("unsupported output format %q (must be table, json, or yaml)", format)
	}
}

func printStructured(cmd *cobra.Command, value any) error {
	format, err := outputFormat(cmd)
	if err != nil {
		return err
	}
	switch format {
	case outputJSON:
		out, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return fmt.Errorf("formatting json output: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(out)) //nolint:errcheck
	case outputYAML:
		out, err := sigsyaml.Marshal(value)
		if err != nil {
			return fmt.Errorf("formatting yaml output: %w", err)
		}
		fmt.Fprint(cmd.OutOrStdout(), string(out)) //nolint:errcheck
	default:
		return printGenericTable(cmd, value)
	}
	return nil
}

func printGenericTable(cmd *cobra.Command, value any) error {
	items := listItems(value)
	if len(items) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No resources found.") //nolint:errcheck
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tNAMESPACE\tSTATUS\tAGE") //nolint:errcheck
	for _, item := range items {
		name := firstString(item, "name", "id")
		if name == "" {
			name = nestedString(item, "metadata", "name")
		}
		namespace := firstString(item, "namespace")
		if namespace == "" {
			namespace = nestedString(item, "metadata", "namespace")
		}
		status := firstString(item, "phase", "status", "state")
		if status == "" {
			status = nestedString(item, "status", "phase")
		}
		age := firstString(item, "createdAt")
		if age == "" {
			age = nestedString(item, "metadata", "creationTimestamp")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", dash(name), dash(namespace), dash(status), dash(formatAge(age))) //nolint:errcheck
	}
	return w.Flush()
}

func readFileOrStdin(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("path is required")
	}
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func manifestJSON(path string) ([]byte, error) {
	data, err := readFileOrStdin(path)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("manifest is empty")
	}
	if json.Valid(trimmed) {
		return trimmed, nil
	}
	jsonBody, err := sigsyaml.YAMLToJSON(trimmed)
	if err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	return jsonBody, nil
}

func manifestMap(path string) (map[string]any, []byte, error) {
	body, err := manifestJSON(path)
	if err != nil {
		return nil, nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, fmt.Errorf("parsing manifest: %w", err)
	}
	return m, body, nil
}

func manifestWithNamespaceJSON(cmd *cobra.Command, path, namespace string) ([]byte, error) {
	m, _, err := manifestMap(path)
	if err != nil {
		return nil, err
	}
	metadata, _ := m["metadata"].(map[string]any)
	metadataNS := ""
	if metadata != nil {
		metadataNS = strings.TrimSpace(anyString(metadata["namespace"]))
	}
	topLevelNS := strings.TrimSpace(anyString(m["namespace"]))
	if metadataNS != "" && topLevelNS != "" && metadataNS != topLevelNS {
		return nil, fmt.Errorf("manifest metadata.namespace %q does not match top-level namespace %q", metadataNS, topLevelNS)
	}
	manifestNS := strings.TrimSpace(manifestNamespace(m))
	flagNS, _ := cmd.Flags().GetString("namespace")
	if strings.TrimSpace(flagNS) != "" && manifestNS != "" && manifestNS != flagNS {
		return nil, fmt.Errorf("manifest namespace %q does not match --namespace %q", manifestNS, flagNS)
	}
	ensureManifestNamespace(m, namespace)
	body, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshaling manifest: %w", err)
	}
	return body, nil
}

func ensureManifestNamespace(m map[string]any, namespace string) {
	if strings.TrimSpace(namespace) == "" || m == nil {
		return
	}
	topLevelNS := strings.TrimSpace(anyString(m["namespace"]))
	metadata, _ := m["metadata"].(map[string]any)
	if metadata != nil {
		if strings.TrimSpace(anyString(metadata["namespace"])) == "" {
			if topLevelNS != "" {
				metadata["namespace"] = topLevelNS
			} else {
				metadata["namespace"] = namespace
			}
		}
		return
	}
	if topLevelNS == "" {
		m["namespace"] = namespace
	}
}

func manifestNamespace(m map[string]any) string {
	metadata, _ := m["metadata"].(map[string]any)
	if metadata != nil {
		if ns := strings.TrimSpace(anyString(metadata["namespace"])); ns != "" {
			return ns
		}
	}
	return anyString(m["namespace"])
}

func namespaceQueryForManifest(
	cmd *cobra.Command,
	clientNamespace string,
	manifest map[string]any,
) (map[string]string, error) {
	manifestNS := strings.TrimSpace(manifestNamespace(manifest))
	if manifestNS == "" {
		return nil, nil
	}
	flagNS, _ := cmd.Flags().GetString("namespace")
	if strings.TrimSpace(flagNS) != "" && flagNS != manifestNS {
		return nil, fmt.Errorf("manifest namespace %q does not match --namespace %q", manifestNS, flagNS)
	}
	if strings.TrimSpace(clientNamespace) != "" && strings.TrimSpace(flagNS) != "" && clientNamespace != manifestNS {
		return nil, fmt.Errorf("manifest namespace %q does not match selected namespace %q", manifestNS, clientNamespace)
	}
	return map[string]string{"namespace": manifestNS}, nil
}

func listItems(value any) []map[string]any {
	if value == nil {
		return nil
	}
	if m, ok := value.(map[string]any); ok {
		if raw, ok := m["items"]; ok {
			return anySliceToMaps(raw)
		}
		if raw, ok := m["data"]; ok {
			return anySliceToMaps(raw)
		}
		return []map[string]any{m}
	}
	return anySliceToMaps(value)
}

func anySliceToMaps(raw any) []map[string]any {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	items := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			items = append(items, m)
		}
	}
	return items
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s := anyString(m[key]); s != "" {
			return s
		}
	}
	return ""
}

func nestedString(m map[string]any, keys ...string) string {
	cur := m
	for i, key := range keys {
		if i == len(keys)-1 {
			return anyString(cur[key])
		}
		next, ok := cur[key].(map[string]any)
		if !ok {
			return ""
		}
		cur = next
	}
	return ""
}

func anyString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case bool:
		return fmt.Sprintf("%t", x)
	default:
		return ""
	}
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" || s == "<unknown>" {
		return "-"
	}
	return s
}

func metadataName(value any) string {
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if s := firstString(m, "name", "id"); s != "" {
		return s
	}
	return nestedString(m, "metadata", "name")
}

func mergeQuery(base map[string]string, pairs ...string) map[string]string {
	q := map[string]string{}
	for k, v := range base {
		if v != "" {
			q[k] = v
		}
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			q[pairs[i]] = pairs[i+1]
		}
	}
	return q
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
