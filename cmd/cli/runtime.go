package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newRuntimePoolCmd() *cobra.Command {
	return newCRUDResourceCmd(crudResourceSpec{
		Use:          "runtime-pool",
		Short:        "Manage controller-owned ACP runtime pools",
		BasePath:     "/api/v1/runtime-pools",
		Name:         "runtime pool",
		ReadOnly:     true,
		TablePrinter: printRuntimePoolTable,
	})
}

func newAgentRuntimeCmd() *cobra.Command {
	return newCRUDResourceCmd(crudResourceSpec{
		Use:          "agent-runtime",
		Short:        "Manage external orka.harness.v2 AgentRuntime registrations",
		BasePath:     "/api/v1/agent-runtimes",
		Name:         "agent runtime",
		TablePrinter: printAgentRuntimeTable,
	})
}

func printRuntimePoolTable(cmd *cobra.Command, value any) error {
	items := listItems(value)
	if len(items) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No runtime pools found.") //nolint:errcheck
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tNAMESPACE\tLIFECYCLE\tADMISSION\tPODS\tSESSIONS\tPROMPTS\tQUEUED\tAGE") //nolint:errcheck
	for _, item := range items {
		status := nestedMap(item, "status")
		capacity := nestedMap(status, "capacity")
		pods := fmt.Sprintf("%s/%s", dash(anyString(status["currentReplicas"])), dash(anyString(status["desiredReplicas"])))
		sessions := fmt.Sprintf("%s/%s", dash(anyString(capacity["residentSessions"])), dash(anyString(capacity["maxResidentSessions"])))
		prompts := fmt.Sprintf("%s/%s", dash(anyString(capacity["runningPrompts"])), dash(anyString(capacity["maxRunningPrompts"])))
		fmt.Fprintf( //nolint:errcheck
			w,
			"%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			dash(nestedString(item, "metadata", "name")),
			dash(nestedString(item, "metadata", "namespace")),
			dash(anyString(status["lifecycle"])),
			dash(anyString(status["admissionState"])),
			pods,
			sessions,
			prompts,
			dash(anyString(capacity["queuedTasks"])),
			dash(formatAge(nestedString(item, "metadata", "creationTimestamp"))),
		) //nolint:errcheck
	}
	return w.Flush()
}

func printAgentRuntimeTable(cmd *cobra.Command, value any) error {
	items := listItems(value)
	if len(items) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No agent runtimes found.") //nolint:errcheck
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tNAMESPACE\tREADY\tCONTRACT\tINTENT\tPROVIDER/MODEL\tINSTANCE\tAGE") //nolint:errcheck
	for _, item := range items {
		spec := nestedMap(item, "spec")
		status := nestedMap(item, "status")
		capabilities := nestedMap(spec, "capabilities")
		profile := nestedMap(capabilities, "profile")
		observed := nestedMap(status, "observedCapabilities")
		provider := firstString(observed, "providerKind")
		if provider == "" {
			provider = firstString(profile, "providerKind")
		}
		model := firstString(observed, "model")
		if model == "" {
			model = firstString(profile, "model")
		}
		instance := firstString(observed, "runtimeInstanceID")
		if instance == "" {
			instance = firstString(capabilities, "runtimeInstanceID")
		}
		fmt.Fprintf( //nolint:errcheck
			w,
			"%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			dash(nestedString(item, "metadata", "name")),
			dash(nestedString(item, "metadata", "namespace")),
			dash(anyString(status["ready"])),
			dash(anyString(spec["contractVersion"])),
			dash(anyString(profile["workspaceIntent"])),
			dash(joinNonEmpty(provider, model, "/")),
			dash(compactCLIValue(instance)),
			dash(formatAge(nestedString(item, "metadata", "creationTimestamp"))),
		) //nolint:errcheck
	}
	return w.Flush()
}

func nestedMap(m map[string]any, keys ...string) map[string]any {
	current := m
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return map[string]any{}
		}
		current = next
	}
	return current
}

func joinNonEmpty(left, right, separator string) string {
	if left == "" {
		return right
	}
	if right == "" {
		return left
	}
	return left + separator + right
}

func compactCLIValue(value string) string {
	const max = 30
	if len(value) <= max {
		return value
	}
	return value[:17] + "…" + value[len(value)-8:]
}
