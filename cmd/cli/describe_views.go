/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// printDescribed prints a single object as a readable field list for table
// output and defers to printStructured for json/yaml, so the structured
// shapes stay byte-for-byte what scripts already parse.
func printDescribed(cmd *cobra.Command, value any, rows func(map[string]any) []describeRow) error {
	format, err := outputFormat(cmd)
	if err != nil {
		return err
	}
	if format != outputTable {
		return printStructured(cmd, value)
	}
	if rows == nil {
		rows = genericDescribeRows
	}
	return printDescribe(cmd, rows(toGenericMap(value)))
}

// taskAgentLabel is what the AGENT column and describe views show for a
// Task: the Agent name for ai/agent Tasks, or the image (last path segment
// and tag) for container Tasks.
func taskAgentLabel(agentRef, image string) string {
	if agentRef = strings.TrimSpace(agentRef); agentRef != "" {
		return agentRef
	}
	return shortImage(image)
}

// shortImage keeps the last path segment of an image reference plus its tag,
// so registry.example.com/team/golang:1.27 prints as golang:1.27. A digest
// reference keeps only its repository name.
func shortImage(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return ""
	}
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	if slash := strings.LastIndex(image, "/"); slash >= 0 {
		image = image[slash+1:]
	}
	return image
}

func taskDescribeRows(task map[string]any) []describeRow {
	spec := nestedMap(task, "spec")
	status := nestedMap(task, "status")
	execution := nestedMap(status, "execution")
	delivery := nestedMap(status, "delivery")
	prompt := firstString(spec, "prompt")
	if prompt == "" {
		prompt = nestedString(spec, "ai", "prompt")
	}
	rows := []describeRow{
		{Label: labelName, Value: nestedString(task, "metadata", "name")},
		{Label: labelNamespace, Value: nestedString(task, "metadata", "namespace")},
		{Label: labelPhase, Value: firstString(status, "phase")},
		{Label: labelType, Value: firstString(spec, "type")},
		{Label: labelAgent, Value: nestedString(spec, "agentRef", "name")},
		{Label: "Image", Value: firstString(spec, "image")},
		{Label: "Model", Value: nestedString(spec, "ai", "model")},
		{Label: labelProvider, Value: firstNonEmpty(nestedString(spec, "ai", "providerRef", "name"), nestedString(spec, "ai", "provider"))},
		{Label: labelSession, Value: nestedString(spec, "sessionRef", "name")},
		{Label: labelCreated, Value: formatTimestamp(nestedString(task, "metadata", "creationTimestamp"))},
		{Label: "Started", Value: formatTimestamp(firstString(status, "startTime"))},
		{Label: "Completed", Value: formatTimestamp(firstString(status, "completionTime"))},
		{Label: labelExecution, Value: joinNonEmpty(firstString(execution, "state"), firstString(execution, "reason"), " / ")},
		{Label: labelDelivery, Value: joinNonEmpty(firstString(delivery, "state"), firstString(delivery, "outcome"), " / ")},
		{Label: labelPublicationBranch, Value: firstString(delivery, "branch")},
		{Label: labelPullRequest, Value: nestedString(delivery, "prReceipt", "url")},
		{Label: labelMessage, Value: firstString(status, "message")},
		{Label: "Prompt", Value: firstLines(prompt, 3)},
	}
	if plan := nestedMap(task, "plan"); len(plan) > 0 {
		rows = append(rows, describeRow{Label: "Plan", Value: firstString(plan, "summary")})
	}
	if result := firstString(task, "result"); result != "" {
		rows = append(rows, describeRow{Label: "Result", Value: firstLines(result, 5)})
	}
	return rows
}

func agentDescribeRows(agent map[string]any) []describeRow {
	spec := nestedMap(agent, "spec")
	status := nestedMap(agent, "status")
	runtime := nestedMap(spec, "runtime")
	runtimeLabel := firstString(runtime, "type")
	if ref := nestedString(runtime, "runtimeRef", "name"); ref != "" {
		runtimeLabel = joinNonEmpty(runtimeLabel, ref, " → ")
	}
	provider := nestedString(spec, "providerRef", "name")
	if provider == "" {
		provider = nestedString(spec, "model", "provider")
	}
	instructions := nestedString(spec, "systemPrompt", "inline")
	if instructions == "" {
		if ref := nestedMap(nestedMap(spec, "systemPrompt"), "configMapRef"); len(ref) > 0 {
			instructions = "from ConfigMap " + joinNonEmpty(firstString(ref, "name"), firstString(ref, "key"), "/")
		}
	}
	return []describeRow{
		{Label: labelName, Value: nestedString(agent, "metadata", "name")},
		{Label: labelNamespace, Value: nestedString(agent, "metadata", "namespace")},
		{Label: "Model", Value: nestedString(spec, "model", "name")},
		{Label: labelProvider, Value: provider},
		{Label: "Runtime", Value: runtimeLabel},
		{Label: labelReady, Value: readinessString(status["ready"])},
		{Label: "Active tasks", Value: anyString(status["activeTasks"])},
		{Label: "Tools", Value: nameList(spec["tools"])},
		{Label: "Skills", Value: nameList(spec["skills"])},
		{Label: "Instructions", Value: instructions},
	}
}

func providerDescribeRows(provider map[string]any) []describeRow {
	spec := nestedMap(provider, "spec")
	status := nestedMap(provider, "status")
	ready := readinessString(status["ready"])
	if _, flat := provider["ready"]; flat && len(status) == 0 {
		// The restricted flat projection served to context-token callers
		// carries readiness at the top level.
		ready = readinessString(provider["ready"])
	}
	return []describeRow{
		{Label: labelName, Value: genericRowName(provider)},
		{Label: labelNamespace, Value: genericRowNamespace(provider)},
		{Label: labelType, Value: firstNonEmpty(firstString(spec, "type"), firstString(provider, "type"))},
		{Label: "Base URL", Value: firstString(spec, "baseURL")},
		{Label: "Default model", Value: firstNonEmpty(firstString(spec, "defaultModel"), firstString(provider, "defaultModel"))},
		{Label: labelReady, Value: ready},
		{Label: labelMessage, Value: firstString(status, "message")},
		{Label: "Last validated", Value: formatTimestamp(firstString(status, "lastValidated"))},
	}
}

func findingLocation(finding map[string]any) string {
	file := firstString(finding, "filePath")
	if file == "" {
		return ""
	}
	if line := anyString(finding["line"]); line != "" && line != "0" {
		return file + ":" + line
	}
	return file
}

func findingDescribeRows(finding map[string]any) []describeRow {
	return []describeRow{
		{Label: labelTitle, Value: firstString(finding, "title")},
		{Label: "Severity", Value: firstString(finding, "severity")},
		{Label: "Validation", Value: firstString(finding, "validationStatus")},
		{Label: labelState, Value: firstString(finding, "state")},
		{Label: "Location", Value: findingLocation(finding)},
		{Label: labelSummary, Value: firstString(finding, "summary")},
		{Label: "Category", Value: firstString(finding, "category")},
		{Label: "Confidence", Value: firstString(finding, "confidence")},
		{Label: labelRepository, Value: firstString(finding, "repositoryScan")},
		{Label: "Scan run", Value: firstString(finding, "scanRunID")},
		{Label: "ID", Value: firstString(finding, "id")},
	}
}

func patchProposalDescribeRows(proposal map[string]any) []describeRow {
	return []describeRow{
		{Label: labelStatus, Value: firstString(proposal, "status")},
		{Label: labelBranch, Value: firstString(proposal, "branch")},
		{Label: labelPullRequest, Value: firstString(proposal, "prURL")},
		{Label: labelTask, Value: firstString(proposal, "taskName")},
		{Label: "Finding", Value: firstString(proposal, "findingID")},
		{Label: labelReason, Value: firstString(proposal, "reason")},
		{Label: labelCreated, Value: formatTimestamp(firstString(proposal, "createdAt"))},
		{Label: "ID", Value: firstString(proposal, "id")},
	}
}

func memoryProposalDescribeRows(proposal map[string]any) []describeRow {
	text := firstString(proposal, "content")
	if text == "" {
		text = firstString(proposal, "description")
	}
	return []describeRow{
		{Label: labelTitle, Value: firstString(proposal, "title")},
		{Label: "Text", Value: text},
		{Label: labelType, Value: firstString(proposal, "type")},
		{Label: labelStatus, Value: firstString(proposal, "status")},
		{Label: "From task", Value: firstString(proposal, "taskName")},
		{Label: labelAgent, Value: firstString(proposal, "agentName")},
		{Label: "Skill", Value: firstString(proposal, "skillName")},
		{Label: "Reviewer", Value: firstString(proposal, "reviewer")},
		{Label: "Review note", Value: firstString(proposal, "reviewNote")},
		{Label: "Applied memory", Value: firstString(proposal, "appliedMemoryId")},
		{Label: "Applied by", Value: firstString(proposal, "appliedBy")},
		{Label: labelCreated, Value: formatTimestamp(firstString(proposal, "createdAt"))},
		{Label: "ID", Value: firstString(proposal, "id")},
	}
}

func memoryDescribeRows(memory map[string]any) []describeRow {
	state := ""
	if disabled, ok := memory["disabled"].(bool); ok && disabled {
		state = "disabled"
	}
	if deleted, ok := memory["deleted"].(bool); ok && deleted {
		state = joinNonEmpty(state, "deleted", ", ")
	}
	return []describeRow{
		{Label: "Content", Value: firstString(memory, "content")},
		{Label: labelSource, Value: firstString(memory, "source")},
		{Label: labelTags, Value: joinStrings(memory["tags"])},
		{Label: labelState, Value: state},
		{Label: labelTask, Value: firstString(memory, "taskName")},
		{Label: labelSession, Value: firstString(memory, "sessionName")},
		{Label: labelAgent, Value: firstString(memory, "agentName")},
		{Label: "Recalled", Value: anyString(memory["recalledCount"])},
		{Label: labelCreated, Value: formatTimestamp(firstString(memory, "createdAt"))},
		{Label: "ID", Value: firstString(memory, "id")},
	}
}

func approvalDescribeRows(approval map[string]any) []describeRow {
	rows := []describeRow{
		{Label: "ID", Value: firstString(approval, "id")},
		{Label: labelStatus, Value: firstString(approval, "status")},
		{Label: "Tool", Value: firstString(approval, "targetTool")},
		{Label: "Action", Value: firstString(approval, "action")},
		{Label: "Severity", Value: firstString(approval, "severity")},
		{Label: "Risk", Value: firstString(approval, "riskSummary")},
	}
	if args, ok := approval["targetArgsPreview"].(map[string]any); ok && len(args) > 0 {
		children := make([]describeRow, 0, len(args))
		for _, key := range sortedAnyKeys(args) {
			children = append(children, describeRow{Label: key, Value: scalarOrJSON(args[key])})
		}
		rows = append(rows, describeRow{Label: "Arguments", Children: children})
	} else if preview := scalarOrJSON(approval["targetArgsPreview"]); preview != "" && preview != "null" {
		rows = append(rows, describeRow{Label: "Arguments", Value: preview})
	}
	rows = append(rows,
		describeRow{Label: "Requested", Value: formatTimestamp(firstString(approval, "createdAt"))},
		describeRow{Label: "Expires", Value: approvalExpiry(approval, time.Now())},
		describeRow{Label: "Decided by", Value: firstString(approval, "decisionActor")},
		describeRow{Label: labelReason, Value: firstString(approval, "decisionReason")},
		describeRow{Label: "Decided", Value: formatTimestamp(firstString(approval, "decisionTime"))},
		describeRow{Label: labelExecution, Value: scalarOrJSON(approval["executionOutcome"])},
	)
	return rows
}

// approvalExpiry shows the time left on a pending request and nothing once
// the request has been decided.
func approvalExpiry(approval map[string]any, now time.Time) string {
	if !strings.EqualFold(firstString(approval, "status"), approvalStatusPending) {
		return ""
	}
	return formatUntil(firstString(approval, "expiresAt"), now)
}

func toolDescribeRows(tool map[string]any) []describeRow {
	spec := nestedMap(tool, "spec")
	httpSpec := nestedMap(spec, "http")
	mcp := nestedMap(spec, "mcp")
	// An MCP Tool may carry http transport settings as well, so the MCP
	// backend is the discriminator when present.
	kind := ""
	switch {
	case len(mcp) > 0:
		kind = "mcp"
	case len(httpSpec) > 0:
		kind = "http"
	}
	if class := firstString(spec, "brokeredToolClass"); class != "" {
		kind = joinNonEmpty(kind, class, " / ")
	}
	// The MCP server is hosted by a workspace class or a Substrate actor
	// template; that reference is its identity.
	mcpServer := ""
	if class := nestedString(mcp, "workspace", "classRef", "name"); class != "" {
		mcpServer = "workspace class " + class
	} else if template := nestedString(mcp, "substrateActor", "templateRef", "name"); template != "" {
		mcpServer = "substrate actor template " + template
	}
	if path := firstString(mcp, "path"); mcpServer != "" && path != "" {
		mcpServer += " (" + path + ")"
	}
	return []describeRow{
		{Label: labelName, Value: genericRowName(tool)},
		{Label: labelNamespace, Value: genericRowNamespace(tool)},
		{Label: labelType, Value: kind},
		{Label: "Method", Value: firstString(httpSpec, "method")},
		{Label: "URL", Value: firstString(httpSpec, "url")},
		{Label: "Outbound policy", Value: nestedString(httpSpec, "outboundAccessPolicyRef", "name")},
		{Label: "MCP server", Value: mcpServer},
		{Label: "Description", Value: firstString(spec, "description")},
	}
}

func agentRuntimeDescribeRows(runtime map[string]any) []describeRow {
	spec := nestedMap(runtime, "spec")
	status := nestedMap(runtime, "status")
	capabilities := nestedMap(spec, "capabilities")
	profile := nestedMap(capabilities, "profile")
	policy := nestedMap(capabilities, "mcpPolicy")
	return []describeRow{
		{Label: labelName, Value: genericRowName(runtime)},
		{Label: labelNamespace, Value: genericRowNamespace(runtime)},
		{Label: labelReady, Value: readinessString(status["ready"])},
		{Label: "Contract", Value: firstString(spec, "contractVersion")},
		{Label: labelProvider, Value: joinNonEmpty(firstString(profile, "providerKind"), firstString(profile, "model"), "/")},
		{Label: "Workspace intent", Value: firstString(profile, "workspaceIntent")},
		{Label: "May call", Value: joinStrings(policy["allowedTools"])},
		{Label: "Needs approval", Value: joinStrings(policy["approvalRequiredTools"])},
		{Label: "Disallowed", Value: joinStrings(policy["disallowedTools"])},
		{Label: labelMessage, Value: firstString(status, "message")},
	}
}

func skillDescribeRows(skill map[string]any) []describeRow {
	spec := nestedMap(skill, "spec")
	status := nestedMap(skill, "status")
	return []describeRow{
		{Label: labelName, Value: genericRowName(skill)},
		{Label: labelNamespace, Value: genericRowNamespace(skill)},
		{Label: "Display name", Value: firstString(spec, "displayName")},
		{Label: labelVersion, Value: firstString(spec, "version")},
		{Label: "Author", Value: firstString(spec, "author")},
		{Label: labelPhase, Value: firstString(status, "phase")},
		{Label: labelTags, Value: joinStrings(spec["tags"])},
		{Label: "Description", Value: firstString(spec, "description")},
	}
}

func sliceDescribeRows(slice map[string]any) []describeRow {
	return []describeRow{
		{Label: labelTitle, Value: firstString(slice, "title")},
		{Label: "Kind", Value: firstString(slice, "kind")},
		{Label: labelStatus, Value: firstString(slice, "status")},
		{Label: "Confidence", Value: firstString(slice, "confidence")},
		{Label: labelSource, Value: firstString(slice, "source")},
		{Label: labelSummary, Value: firstString(slice, "summary")},
		{Label: "Entrypoints", Value: sliceFileList(slice["entrypoints"])},
		{Label: "Owned files", Value: sliceFileList(slice["ownedFiles"])},
		{Label: labelTags, Value: joinStrings(slice["tags"])},
		{Label: "Last reviewed", Value: formatTimestamp(firstString(slice, "lastReviewedAt"))},
		{Label: "Last scan run", Value: firstString(slice, "lastScanRunID")},
		{Label: "ID", Value: firstString(slice, "id")},
	}
}

func sliceFileList(value any) string {
	list, ok := value.([]any)
	if !ok {
		return ""
	}
	paths := make([]string, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			if path := firstString(m, "path"); path != "" {
				paths = append(paths, path)
			}
		}
	}
	return strings.Join(paths, "\n")
}

func planDescribeRows(plan map[string]any) []describeRow {
	progress := anyString(plan["ProgressPct"])
	if progress != "" {
		progress += "%"
	}
	return []describeRow{
		{Label: labelTask, Value: firstString(plan, "TaskName")},
		{Label: "Iteration", Value: anyString(plan["Iteration"])},
		{Label: "Progress", Value: progress},
		{Label: "Goal complete", Value: anyString(plan["GoalComplete"])},
		{Label: labelSummary, Value: firstString(plan, "Summary")},
		{Label: labelUpdated, Value: formatTimestamp(firstString(plan, "UpdatedAt"))},
		{Label: "Plan", Value: firstString(plan, "PlanDocument")},
	}
}

func repositoryScanDescribeRows(scan map[string]any) []describeRow {
	spec := nestedMap(scan, "spec")
	status := nestedMap(scan, "status")
	counts := nestedMap(status, "findingCounts")
	return []describeRow{
		{Label: labelName, Value: genericRowName(scan)},
		{Label: labelNamespace, Value: genericRowNamespace(scan)},
		{Label: labelRepository, Value: firstString(spec, "repoURL")},
		{Label: labelBranch, Value: firstString(spec, "branch")},
		{Label: "Schedule", Value: firstString(spec, "schedule")},
		{Label: "Validation mode", Value: firstString(spec, "validationMode")},
		{Label: "Analysis agent", Value: nestedString(spec, "analysisAgentRef", "name")},
		{Label: labelPhase, Value: firstString(status, "phase")},
		{Label: "Last scan", Value: firstString(status, "lastScanID")},
		{Label: "Last scan at", Value: formatTimestamp(firstString(status, "lastScanAt"))},
		{Label: "Findings", Value: keyValuePairs(counts)},
	}
}

func monitorDescribeRows(monitor map[string]any) []describeRow {
	spec := nestedMap(monitor, "spec")
	status := nestedMap(monitor, "status")
	return []describeRow{
		{Label: labelName, Value: genericRowName(monitor)},
		{Label: labelNamespace, Value: genericRowNamespace(monitor)},
		{Label: labelRepository, Value: firstString(spec, "repoURL")},
		{Label: labelBranch, Value: firstString(spec, "branch")},
		{Label: labelPhase, Value: firstString(status, "phase")},
		{Label: "Last run", Value: firstString(status, "lastRunID")},
		{Label: "Last run at", Value: formatTimestamp(firstString(status, "lastRunTime"))},
		{Label: "Open pull requests", Value: anyString(status["openPullRequests"])},
		{Label: "Pending reviews", Value: anyString(status["pendingReviews"])},
	}
}

func sessionDescribeRows(session map[string]any) []describeRow {
	return []describeRow{
		{Label: labelName, Value: firstString(session, "Name", "name")},
		{Label: labelNamespace, Value: firstString(session, "Namespace", "namespace")},
		{Label: labelType, Value: firstString(session, "SessionType", "sessionType", "type")},
		{Label: "Active task", Value: firstString(session, "ActiveTask", "activeTask")},
		{Label: "Messages", Value: anyString(firstNonNil(session, "MessageCount", "messageCount"))},
		{Label: "Tokens", Value: joinNonEmpty(anyString(firstNonNil(session, "InputTokens", "inputTokens")), anyString(firstNonNil(session, "OutputTokens", "outputTokens")), " in / ") + tokenSuffix(session)},
		{Label: "Cancelled", Value: boolString(firstNonNil(session, "Cancelled", "cancelled"))},
		{Label: labelCreated, Value: formatTimestamp(firstString(session, "CreatedAt", "createdAt"))},
		{Label: labelUpdated, Value: formatTimestamp(firstString(session, "UpdatedAt", "updatedAt"))},
	}
}

func tokenSuffix(session map[string]any) string {
	if anyString(firstNonNil(session, "OutputTokens", "outputTokens")) != "" {
		return " out"
	}
	return ""
}

func gatewayEventDescribeRows(event map[string]any) []describeRow {
	return []describeRow{
		{Label: "ID", Value: firstString(event, "id")},
		{Label: labelState, Value: joinNonEmpty(firstString(event, "state"), firstString(event, "stateMessage"), ": ")},
		{Label: labelGateway, Value: firstString(event, "gatewayName")},
		{Label: "Binding", Value: firstString(event, "bindingName")},
		{Label: labelAgent, Value: firstString(event, "agentName")},
		{Label: labelSession, Value: firstString(event, "sessionName")},
		{Label: "Event type", Value: firstString(event, "eventType")},
		{Label: "Sender", Value: joinNonEmpty(firstString(event, "senderDisplayName"), firstString(event, "senderId"), " ")},
		{Label: "Text", Value: firstString(event, "text")},
		{Label: "Received", Value: formatTimestamp(firstString(event, "receivedAt", "createdAt"))},
	}
}

func gatewayDeliveryDescribeRows(delivery map[string]any) []describeRow {
	return []describeRow{
		{Label: "ID", Value: firstString(delivery, "id")},
		{Label: labelState, Value: joinNonEmpty(firstString(delivery, "state"), firstString(delivery, "stateMessage", "lastError"), ": ")},
		{Label: "Kind", Value: firstString(delivery, "kind")},
		{Label: labelGateway, Value: firstString(delivery, "gatewayName")},
		{Label: "Binding", Value: firstString(delivery, "bindingName")},
		{Label: "Event", Value: firstString(delivery, "eventId")},
		{Label: labelTask, Value: firstString(delivery, "taskName")},
		{Label: labelSession, Value: firstString(delivery, "sessionName")},
		{Label: "Attempts", Value: joinNonEmpty(anyString(delivery["attemptCount"]), anyString(delivery["maxAttempts"]), " of ")},
		{Label: "Next attempt", Value: formatTimestamp(firstString(delivery, "nextAttemptAt"))},
		{Label: labelUpdated, Value: formatTimestamp(firstString(delivery, "updatedAt"))},
	}
}

func gatewayDescribeRows(gateway map[string]any) []describeRow {
	spec := nestedMap(gateway, "spec")
	status := nestedMap(gateway, "status")
	observed := nestedMap(status, "observedCapabilities")
	return []describeRow{
		{Label: labelName, Value: genericRowName(gateway)},
		{Label: labelNamespace, Value: genericRowNamespace(gateway)},
		{Label: "Class", Value: firstString(spec, "gatewayClassName")},
		{Label: "Adapter", Value: joinNonEmpty(firstString(observed, "adapterName"), firstString(observed, "adapterVersion"), " ")},
		{Label: "Endpoint", Value: firstString(status, "resolvedEndpoint")},
		{Label: labelAccepted, Value: readinessString(status["accepted"])},
		{Label: "Resolved refs", Value: readinessString(status["resolvedRefs"])},
		{Label: "Connected", Value: readinessString(status["connected"])},
		{Label: labelReady, Value: readinessString(status["ready"])},
		{Label: labelMessage, Value: firstString(status, "message")},
		{Label: labelCreated, Value: formatTimestamp(nestedString(gateway, "metadata", "creationTimestamp"))},
	}
}

func gatewayClassDescribeRows(class map[string]any) []describeRow {
	spec := nestedMap(class, "spec")
	status := nestedMap(class, "status")
	return []describeRow{
		{Label: labelName, Value: genericRowName(class)},
		{Label: "Contract", Value: firstString(spec, "contractVersion")},
		{Label: "Category", Value: firstString(spec, "category")},
		{Label: labelAccepted, Value: readinessString(status["accepted"])},
		{Label: labelMessage, Value: firstString(status, "message")},
		{Label: labelCreated, Value: formatTimestamp(nestedString(class, "metadata", "creationTimestamp"))},
	}
}

func gatewayBindingDescribeRows(binding map[string]any) []describeRow {
	spec := nestedMap(binding, "spec")
	status := nestedMap(binding, "status")
	return []describeRow{
		{Label: labelName, Value: genericRowName(binding)},
		{Label: labelNamespace, Value: genericRowNamespace(binding)},
		{Label: labelGateway, Value: nestedString(spec, "gatewayRef", "name")},
		{Label: labelAgent, Value: nestedString(spec, "agentRef", "name")},
		{Label: "Priority", Value: anyString(spec["priority"])},
		{Label: labelAccepted, Value: readinessString(status["accepted"])},
		{Label: "Resolved refs", Value: readinessString(status["resolvedRefs"])},
		{Label: "Programmed", Value: readinessString(status["programmed"])},
		{Label: labelReady, Value: readinessString(status["ready"])},
		{Label: labelMessage, Value: firstString(status, "message")},
		{Label: labelCreated, Value: formatTimestamp(nestedString(binding, "metadata", "creationTimestamp"))},
	}
}

func whoamiDescribeRows(identity map[string]any) []describeRow {
	rows := []describeRow{
		{Label: "User", Value: firstString(identity, "username", "subject")},
		{Label: "Authenticated", Value: boolString(identity["authenticated"])},
		{Label: "Auth type", Value: firstString(identity, "authType")},
		{Label: "UID", Value: firstString(identity, "uid")},
		{Label: "Groups", Value: joinStrings(identity["groups"])},
		{Label: "Roles", Value: joinStrings(identity["roles"])},
		{Label: labelNamespace, Value: firstString(identity, "namespace")},
		{Label: "Email", Value: firstString(identity, "email")},
		{Label: "Issuer", Value: firstString(identity, "issuer")},
	}
	if transaction := nestedMap(identity, "transaction"); len(transaction) > 0 {
		rows = append(rows, describeRow{Label: "Transaction", Children: []describeRow{
			{Label: "ID", Value: firstString(transaction, "id")},
			{Label: labelType, Value: firstString(transaction, "type")},
			{Label: "Profile", Value: firstString(transaction, "profile")},
			{Label: "Scope", Value: firstString(transaction, "scope")},
			{Label: "Subject", Value: firstString(transaction, "subject")},
			{Label: "Issuer", Value: firstString(transaction, "issuer")},
			{Label: "Workload", Value: firstString(transaction, "requestingWorkload")},
		}})
	}
	return rows
}

// workspaceStatusRows prints the safe workspace status one field per line,
// identity and phase first, then the workspace, execution workspace, and
// delivery sections.
func workspaceStatusRows(status map[string]any) []describeRow {
	return orderedFlatRows(status, "task", "namespace", "phase", "workspace", "executionWorkspace", "delivery")
}

// orderedFlatRows is flatDescribeRows with the named keys first, in the
// order given, and the remaining keys sorted after them.
func orderedFlatRows(object map[string]any, preferred ...string) []describeRow {
	ordered := map[string]any{}
	rows := make([]describeRow, 0, len(object))
	for _, key := range preferred {
		if value, ok := object[key]; ok {
			rows = append(rows, flatDescribeRows(map[string]any{key: value})...)
			ordered[key] = value
		}
	}
	rest := map[string]any{}
	for key, value := range object {
		if _, done := ordered[key]; !done {
			rest[key] = value
		}
	}
	return append(rows, flatDescribeRows(rest)...)
}

// flatDescribeRows renders every scalar field of an object in key order and
// nested objects as indented sections, for views such as workspace status
// whose fields are already a curated safe subset.
func flatDescribeRows(object map[string]any) []describeRow {
	rows := make([]describeRow, 0, len(object))
	for _, key := range sortedAnyKeys(object) {
		switch value := object[key].(type) {
		case map[string]any:
			rows = append(rows, describeRow{Label: key, Children: flatDescribeRows(value)})
		case nil:
			continue
		default:
			rows = append(rows, describeRow{Label: key, Value: scalarOrJSON(value)})
		}
	}
	return rows
}

// readinessString renders a readiness boolean, treating an absent value as
// false: the API omits `ready` when it is false, and a missing Ready row
// would hide the one negative state a person checks for.
func readinessString(value any) string {
	if b, ok := value.(bool); ok && b {
		return "true"
	}
	return "false"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonNil(m map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := m[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

func boolString(value any) string {
	if b, ok := value.(bool); ok {
		return anyString(b)
	}
	return ""
}

func sortedAnyKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}

// printListWith prints a list (or any table-shaped response) with a
// dedicated table printer for table output and defers to printStructured
// otherwise.
func printListWith(cmd *cobra.Command, value any, table func(*cobra.Command, any) error) error {
	format, err := outputFormat(cmd)
	if err != nil {
		return err
	}
	if format != outputTable {
		return printStructured(cmd, value)
	}
	return table(cmd, value)
}

// printPullRequestReceipt prints the pull request URL a finding's patch was
// published to.
func printPullRequestReceipt(cmd *cobra.Command, value any) error {
	receipt := toGenericMap(value)
	url := firstString(receipt, "prURL")
	if url == "" {
		return printDescribe(cmd, flatDescribeRows(receipt))
	}
	fmt.Fprintln(cmd.OutOrStdout(), url) //nolint:errcheck
	return nil
}
