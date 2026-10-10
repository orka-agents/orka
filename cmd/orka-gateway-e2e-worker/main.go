/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Test-only deterministic native worker. It executes the production reply tool
// and client with the real projected Pod token, not a model or an auth bypass.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/gateway/workerclient"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	fixtureDirectory = "/tmp/gateway-e2e-"
	interimText      = "Gateway interim E2E message"
	finalText        = "Gateway interim E2E final"
)

type workerReport struct {
	First     tools.GatewayReplyReceipt `json:"first"`
	Replay    tools.GatewayReplyReceipt `json:"replay"`
	ErrorCode string                    `json:"errorCode,omitempty"`
}

type workerConfig struct {
	controllerURL, namespace, task, taskUID, tokenFile string
	capable                                            bool
	client                                             *http.Client
	retryInterval                                      time.Duration
}

func runWorker(
	ctx context.Context, cfg workerConfig, ready func(workerReport) error, release func(context.Context) error,
) error {
	sender, err := workerclient.New(workerclient.Config{
		ControllerURL: cfg.controllerURL, Namespace: cfg.namespace, TaskName: cfg.task,
		TaskUID: cfg.taskUID, TokenFile: cfg.tokenFile,
	})
	if err != nil {
		return err
	}
	if err := sender.AuthenticateOrigin(ctx); err != nil {
		return err
	}
	// This deterministic fixture makes one logical call per Task UID. Keep its
	// host-owned identity fixed across replay; it is never a model argument.
	const hostOperationID = "gateway-e2e-interim-call"
	toolCtx := tools.WithToolContext(ctx, &tools.ToolContext{
		Namespace: cfg.namespace, TaskID: cfg.task, TaskUID: cfg.taskUID,
		OperationID: hostOperationID, GatewayReplySender: sender,
	})
	tool := tools.NewReplyInConversationTool()
	args, _ := json.Marshal(map[string]string{"content": interimText})
	result, err := tool.Execute(toolCtx, args)
	report := workerReport{}
	if cfg.capable {
		if err != nil {
			return err
		}
		if report.First, err = parseReceipt(result); err != nil || !report.First.Created {
			return errors.New("initial message receipt invalid")
		}
		result, err = tool.Execute(toolCtx, args)
		if err != nil {
			return err
		}
		if report.Replay, err = parseReceipt(result); err != nil || report.Replay.Created ||
			report.Replay.DeliveryID != report.First.DeliveryID {
			return errors.New("message replay receipt invalid")
		}
	} else {
		rejection, ok := errors.AsType[*tools.GatewayReplyRejection](err)
		if !ok || rejection.Error() != tools.NewGatewayReplyRejection("interim_delivery_unsupported", nil).Error() {
			return errors.New("expected typed unsupported capability rejection from reply tool")
		}
		report.ErrorCode = "interim_delivery_unsupported"
	}
	if err := ready(report); err != nil {
		return err
	}
	// E2E inspects the durable receipt and Running Task before releasing final.
	if err := release(ctx); err != nil {
		return err
	}
	resultPath := "/internal/v1/results/" + url.PathEscape(cfg.namespace) + "/" + url.PathEscape(cfg.task)
	_, status, err := cfg.post(ctx, resultPath, []byte(finalText))
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("result submission returned HTTP %d", status)
	}
	return nil
}

func parseReceipt(result string) (tools.GatewayReplyReceipt, error) {
	var envelope struct {
		Success bool                      `json:"success"`
		Data    tools.GatewayReplyReceipt `json:"data"`
	}
	if json.Unmarshal([]byte(result), &envelope) != nil || !envelope.Success ||
		envelope.Data.DeliveryID == "" || envelope.Data.Status == "" {
		return tools.GatewayReplyReceipt{}, errors.New("invalid reply tool success receipt")
	}
	return envelope.Data, nil
}

// Only final-result publication uses the fixture transport. Interim calls go
// through the production tool and workerclient, including its bootstrap retry.
func (cfg workerConfig) post(ctx context.Context, path string, body []byte) ([]byte, int, error) {
	for {
		token, err := workerenv.ReadTokenFile(cfg.tokenFile, "projected worker token")
		if err != nil {
			return nil, 0, errors.New("projected worker token unavailable")
		}
		endpoint := strings.TrimRight(cfg.controllerURL, "/") + path
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, 0, errors.New("invalid fixture controller URL")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response, err := cfg.client.Do(req)
		if err != nil {
			return nil, 0, errors.New("fixture controller request failed")
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
		_ = response.Body.Close()
		if readErr != nil || len(data) > 65536 {
			return nil, 0, errors.New("invalid fixture controller response")
		}
		if response.StatusCode != http.StatusServiceUnavailable {
			return data, response.StatusCode, nil
		}
		// Preserve the final publisher's transient-503 retry; never retry an
		// authorization failure. Message replay belongs to workerclient above.
		if err := wait(ctx, cfg.retryInterval); err != nil {
			return nil, 0, err
		}
	}
}

func wait(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func waitForFile(ctx context.Context, name string) error {
	for {
		if _, err := os.Stat(fixtureDirectory + name); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := wait(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

func main() {
	mode := flag.String("mode", "ai", "fixture worker mode")
	start := flag.Bool("start", false, "release the test-only start fence through kubectl exec")
	release := flag.Bool("release", false, "release the test-only final fence through kubectl exec")
	inspect := flag.Bool("inspect", false, "print only the safe fixture receipt")
	flag.Parse()
	if *start || *release {
		name := "start"
		if *release {
			name = "release"
		}
		if err := os.WriteFile(fixtureDirectory+name, []byte("ready"), 0600); err != nil {
			os.Exit(1)
		}
		return
	}
	if *inspect {
		data, err := os.ReadFile(fixtureDirectory + "receipt")
		if err != nil {
			os.Exit(1)
		}
		_, _ = os.Stdout.Write(data)
		return
	}
	agent := os.Getenv(workerenv.AgentName)
	if *mode != "ai" || (agent != "gateway-e2e-native" && agent != "gateway-e2e-native-legacy") {
		fmt.Fprintln(os.Stderr, "unsupported fixture worker configuration")
		os.Exit(1)
	}
	cfg := workerConfig{
		controllerURL: os.Getenv(workerenv.ControllerURL), namespace: os.Getenv(workerenv.TaskNamespace),
		task: os.Getenv(workerenv.TaskName), taskUID: os.Getenv(workerenv.TaskUID),
		tokenFile: workerenv.ServiceAccountTokenFile,
		capable:   agent == "gateway-e2e-native", retryInterval: time.Second,
		client: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err := waitForFile(ctx, "start")
	if err == nil {
		err = runWorker(ctx, cfg, func(report workerReport) error {
			data, err := json.Marshal(report)
			if err != nil {
				return err
			}
			if err := os.WriteFile(fixtureDirectory+"receipt.tmp", data, 0600); err != nil {
				return err
			}
			return os.Rename(fixtureDirectory+"receipt.tmp", fixtureDirectory+"receipt")
		}, func(ctx context.Context) error { return waitForFile(ctx, "release") })
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gateway E2E worker failed:", err)
		os.Exit(1)
	}
}
