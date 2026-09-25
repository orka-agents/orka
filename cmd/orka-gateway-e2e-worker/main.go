/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Test-only deterministic native worker. It uses the real projected Pod token
// and controller endpoints; it is not an agent-facing tool or an auth bypass.
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
)

const (
	fixtureDirectory = "/tmp/gateway-e2e-"
	interimText      = "Gateway interim E2E message"
	finalText        = "Gateway interim E2E final"
)

type messageReceipt struct {
	DeliveryID string `json:"deliveryID"`
	Status     string `json:"status"`
	Created    bool   `json:"created"`
}

type workerReport struct {
	First     messageReceipt `json:"first"`
	Replay    messageReceipt `json:"replay"`
	ErrorCode string         `json:"errorCode,omitempty"`
}

type workerConfig struct {
	controllerURL, namespace, task, token string
	capable                               bool
	client                                *http.Client
	retryInterval                         time.Duration
}

func runWorker(
	ctx context.Context, cfg workerConfig, ready func(workerReport) error, release func(context.Context) error,
) error {
	path := "/internal/v1/tasks/" + url.PathEscape(cfg.namespace) + "/" + url.PathEscape(cfg.task) + "/gateway-messages"
	body, _ := json.Marshal(struct {
		Content   string `json:"content"`
		RequestID string `json:"requestID"`
	}{interimText, "gateway-e2e-message"})
	data, status, err := cfg.post(ctx, path, body)
	if err != nil {
		return err
	}
	report := workerReport{}
	if cfg.capable {
		if status != http.StatusAccepted || json.Unmarshal(data, &report.First) != nil ||
			!report.First.Created || report.First.DeliveryID == "" || report.First.Status == "" {
			return errors.New("initial message receipt invalid")
		}
		data, status, err = cfg.post(ctx, path, body)
		if err != nil {
			return err
		}
		if status != http.StatusOK || json.Unmarshal(data, &report.Replay) != nil || report.Replay.Created ||
			report.Replay.DeliveryID != report.First.DeliveryID || report.Replay.Status == "" {
			return errors.New("message replay receipt invalid")
		}
	} else {
		var rejection struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if status != http.StatusConflict || json.Unmarshal(data, &rejection) != nil ||
			rejection.Error.Code != "interim_delivery_unsupported" {
			return errors.New("expected explicit unsupported capability rejection")
		}
		report.ErrorCode = rejection.Error.Code
	}
	if err := ready(report); err != nil {
		return err
	}
	// E2E inspects the durable receipt and Running Task before releasing final.
	if err := release(ctx); err != nil {
		return err
	}
	resultPath := "/internal/v1/results/" + url.PathEscape(cfg.namespace) + "/" + url.PathEscape(cfg.task)
	_, status, err = cfg.post(ctx, resultPath, []byte(finalText))
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("result submission returned HTTP %d", status)
	}
	return nil
}

func (cfg workerConfig) post(ctx context.Context, path string, body []byte) ([]byte, int, error) {
	for {
		endpoint := strings.TrimRight(cfg.controllerURL, "/") + path
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, 0, errors.New("invalid fixture controller URL")
		}
		req.Header.Set("Authorization", "Bearer "+cfg.token)
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
		// A fast real worker can precede publication of its Job identity. Retry only
		// transient 503, with the same request ID; never retry authorization failures.
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
	agent := os.Getenv("ORKA_AGENT_NAME")
	if *mode != "ai" || (agent != "gateway-e2e-native" && agent != "gateway-e2e-native-legacy") {
		fmt.Fprintln(os.Stderr, "unsupported fixture worker configuration")
		os.Exit(1)
	}
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil || len(bytes.TrimSpace(token)) == 0 {
		fmt.Fprintln(os.Stderr, "projected worker token unavailable")
		os.Exit(1)
	}
	cfg := workerConfig{
		controllerURL: os.Getenv("ORKA_CONTROLLER_URL"), namespace: os.Getenv("ORKA_TASK_NAMESPACE"),
		task: os.Getenv("ORKA_TASK_NAME"), token: strings.TrimSpace(string(token)),
		capable: agent == "gateway-e2e-native", retryInterval: time.Second,
		client: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err = waitForFile(ctx, "start")
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
