/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orka-agents/orka/internal/connectors"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// brokeredResultBudget keeps a tool result that can grow with the size of
// a pull request or issue discussion under the broker's result limit, with
// room for the result envelope.
const brokeredResultBudget = harnessv2.MaxMCPResultBytes - 16*1024

// RegisterBrokeredGitHubTools registers the GitHub built-ins that can run
// under a person's linked account into the controller MCP broker registry.
// ACP runtimes reach them only through the requester's Connection: the
// broker binds the link frozen at dispatch, hides the tools when there is
// none, and never resolves a Task credential Secret for them. The set is
// exactly the connector built-in catalog, so a provider can declare every
// tool registered here and nothing else. Registration is idempotent.
func RegisterBrokeredGitHubTools(r *Registry, k8sClient client.Client) error {
	if r == nil {
		return fmt.Errorf("brokered GitHub tool registry is required")
	}
	if k8sClient == nil {
		return fmt.Errorf("brokered GitHub tools require a Kubernetes client")
	}
	registered := map[string]Tool{}
	for _, tool := range []Tool{
		NewCheckPullRequestCITool(k8sClient),
		NewGetIssueTool(k8sClient).WithMaxResultBytes(brokeredResultBudget),
		NewListIssuesTool(k8sClient),
		NewListPullRequestsTool(k8sClient),
		NewReviewPullRequestTool(k8sClient).WithMaxResultBytes(brokeredResultBudget),
		NewCommentOnIssueTool(k8sClient),
		NewCreatePullRequestTool(k8sClient),
		NewPostReviewCommentTool(k8sClient),
	} {
		registered[tool.Name()] = tool
	}
	for _, name := range connectors.BuiltinConnectorToolNames() {
		tool, ok := registered[name]
		if !ok {
			return fmt.Errorf("connector built-in %q has no brokered implementation", name)
		}
		r.Register(tool)
		delete(registered, name)
	}
	if len(registered) > 0 {
		return fmt.Errorf("brokered GitHub tools outside the connector catalog: %d", len(registered))
	}
	return nil
}
