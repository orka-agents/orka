package main

import (
	"testing"

	"github.com/orka-agents/orka/internal/controller"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
)

func TestWorkspacePublicationReclaimerWithoutPublisher(t *testing.T) {
	t.Setenv("ORKA_WORKSPACE_PUBLISHER_URL", "")
	t.Setenv("ORKA_ACP_ARTIFACT_CAPABILITY_SECRET_FILE", "")

	publisherClient, _, _, err := workspacePublisherClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &controller.TaskReconciler{
		ACPPublicationReclaimer: workspacePublicationReclaimer(publisherClient),
	}
	if reconciler.ACPPublicationReclaimer != nil {
		t.Fatal("disabled Publisher must not install a Task publication reclaimer")
	}
}

func TestWorkspacePublicationReclaimerPreservesConfiguredClient(t *testing.T) {
	t.Parallel()
	publisherClient := new(publisherservice.Client)
	if got := workspacePublicationReclaimer(publisherClient); got != publisherClient {
		t.Fatal("configured Publisher must remain the exact Task publication reclaimer")
	}
}
