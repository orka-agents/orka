/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/google/uuid"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/conformance/conformancetest"
)

func main() {
	runtimeName := requiredEnv("ORKA_E2E_RUNTIME_NAME")
	epochValue := requiredEnv("ORKA_E2E_CONTROLLER_EPOCH")
	epoch, err := strconv.ParseUint(epochValue, 10, 64)
	if err != nil || epoch == 0 {
		log.Fatalf("invalid ORKA_E2E_CONTROLLER_EPOCH %q", epochValue)
	}

	profile, err := conformancetest.DeterministicProfile(runtimeName)
	if err != nil {
		log.Fatal(err)
	}
	captureBrokerPrompt := os.Getenv("ORKA_E2E_CAPTURE_BROKER_PROMPT") == "true"
	address := ":8080"
	if captureBrokerPrompt {
		profile, err = brokerProbeProfile(profile)
		if err != nil {
			log.Fatal("invalid broker probe profile")
		}
		address = "" // A private backend; the observer proxy owns the public port.
	}

	server, err := conformancetest.NewServer(conformancetest.Config{
		ListenAddress:                 address,
		ControllerBearerToken:         requiredEnv("ORKA_E2E_CONTROLLER_TOKEN"),
		OperationCapabilitySecret:     []byte(requiredEnv("ORKA_E2E_CAPABILITY_SECRET")),
		ControllerEpoch:               epoch,
		RuntimeInstanceID:             harnessv2.RuntimeInstanceID(runtimeName),
		SupervisorBootID:              harnessv2.SupervisorBootID(uuid.NewString()),
		RuntimePoolUID:                harnessv2.RuntimePoolUID(runtimeName + "-pool"),
		Profile:                       profile,
		Limits:                        harnessv2.DefaultProtocolLimits(),
		SupportsDrain:                 true,
		WorkspaceGovernance:           harnessv2.StrictWorkspaceGovernanceCapabilities(),
		CompleteNonConformancePrompts: !captureBrokerPrompt,
		PromptResultText:              conformancetest.DeterministicPromptResult,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()

	if captureBrokerPrompt {
		recorder, recorderErr := newBrokerPromptRecorder(server.URL(), requiredEnv("ORKA_E2E_CONTROLLER_TOKEN"))
		if recorderErr != nil {
			log.Fatal("broker fixture observer initialization failed")
		}
		if modelURL := os.Getenv("ORKA_E2E_BROKER_MODEL_URL"); modelURL != "" {
			recorder.loopConfig = &brokerLoopConfig{modelURL: modelURL,
				brokerURL: requiredEnv("ORKA_E2E_BROKER_API_URL"), namespace: requiredEnv("ORKA_E2E_BROKER_NAMESPACE"),
				capability: []byte(requiredEnv("ORKA_E2E_CAPABILITY_SECRET"))}
		}
		observer := &http.Server{Addr: ":8080", Handler: recorder, ReadHeaderTimeout: 5 * time.Second}
		defer func() { _ = observer.Close() }()
		go func() {
			if serveErr := observer.ListenAndServe(); serveErr != nil && serveErr != http.ErrServerClosed {
				log.Fatal("broker fixture observer stopped")
			}
		}()
	}

	log.Printf("harness-v2 E2E fixture listening for runtime %q at epoch %d", runtimeName, epoch)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
}

func requiredEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}
