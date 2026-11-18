//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"k8s.io/client-go/rest"
)

func runBoundary(ctx context.Context, output io.Writer) error {
	file, err := openProjected(pv.ApplicationInputPath)
	if err != nil {
		return err
	}
	input, readErr := pv.DecodeApplicationBoundaryInput(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || input.TaskUID != os.Getenv("ORKA_APPLICATION_TASK_UID") ||
		os.Getenv("ORKA_APPLICATION_POD_UID") == "" {
		return pv.ErrBinding
	}
	report, err := measureProcess()
	if err != nil {
		return err
	}
	report.Version, report.TaskUID = pv.ApplicationProtocolVersion, input.TaskUID
	report.PodUID = os.Getenv("ORKA_APPLICATION_POD_UID")
	report.InputDigest, err = pv.ApplicationBoundaryInputDigest(input)
	if err != nil {
		return err
	}
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	connection, connectionErr := (&net.Dialer{}).DialContext(probe, "tcp",
		net.JoinHostPort(input.CanaryHost, strconv.Itoa(input.CanaryPort)))
	report.CanaryReachable = connectionErr == nil
	if connection != nil {
		_ = connection.Close()
	}
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if input.PositiveControl && input.SourceHost != "" {
		status, err := sourceHTTPSProbe(ctx, input)
		if err != nil {
			return err
		}
		report.SourceHTTPSVerified, report.SourceHTTPStatus = true, status
	}
	if input.APIAddress != "" {
		if err := observeBoundaryAPI(ctx, input, &report); err != nil {
			return err
		}
	} else if err := noTargetCredentials(); err != nil {
		return err
	}
	if err := pv.ValidateApplicationBoundaryReport(input, report); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeReport(output, report)
}

func observeBoundaryAPI(
	ctx context.Context, input pv.ApplicationBoundaryInput, report *pv.ApplicationBoundaryReport,
) error {
	config, err := targetConfig(input.APIAddress)
	if err != nil {
		return err
	}
	transport, err := rest.TransportFor(config)
	if err != nil {
		return errors.New("application target authentication is unavailable")
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	report.APIStatuses = map[string]int{}
	status, body, err := boundaryRequest(ctx, client, input.APIAddress, http.MethodPost,
		"/apis/authentication.k8s.io/v1/selfsubjectreviews",
		[]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`))
	if err != nil || status != http.StatusCreated {
		return errors.New("application test identity could not be authenticated by the target API")
	}
	var review struct {
		Status struct {
			UserInfo struct {
				Username string `json:"username"`
				UID      string `json:"uid"`
			} `json:"userInfo"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &review) != nil ||
		review.Status.UserInfo.Username != "system:serviceaccount:"+input.Target.Namespace+":"+input.ServiceAccountName ||
		review.Status.UserInfo.UID == "" {
		return errors.New("application target authenticated a different identity")
	}
	report.APIStatuses["self-identity"], report.APIUserUID = status, review.Status.UserInfo.UID
	if input.PositiveControl {
		podName := os.Getenv("ORKA_APPLICATION_POD_NAME")
		if podName == "" {
			return pv.ErrBinding
		}
		status, body, err := boundaryRequest(ctx, client, input.APIAddress, http.MethodGet,
			"/api/v1/namespaces/"+input.Target.Namespace+"/pods/"+podName, nil)
		var pod struct {
			Metadata struct {
				UID string `json:"uid"`
			} `json:"metadata"`
		}
		if err != nil || status != 200 || json.Unmarshal(body, &pod) != nil || pod.Metadata.UID != report.PodUID {
			return errors.New("application positive API control did not return the exact probe Pod")
		}
		report.APIStatuses["self-pod"] = status
	}
	requests := []struct {
		name, method, path string
		body               []byte
	}{
		{"foreign-namespace", http.MethodGet,
			"/api/v1/namespaces/kube-system/configmaps/extension-apiserver-authentication", nil},
		{"secrets", http.MethodGet, "/api/v1/namespaces/kube-system/secrets", nil},
		{"cluster-list", http.MethodGet, "/api/v1/namespaces", nil},
		{"token-request", http.MethodPost,
			"/api/v1/namespaces/" + input.Target.Namespace + "/serviceaccounts/" + input.ServiceAccountName + "/token",
			[]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"TokenRequest",` +
				`"spec":{"audiences":["orka.ai/application-boundary"],"expirationSeconds":600}}`)},
	}
	for _, request := range requests {
		status, _, err := boundaryRequest(ctx, client, input.APIAddress, request.method, request.path, request.body)
		if err != nil || status != http.StatusForbidden {
			return errors.New("application target identity denial was not established")
		}
		report.APIStatuses[request.name] = status
	}
	return nil
}

func boundaryRequest(
	ctx context.Context, client *http.Client, server, method, path string, body []byte,
) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, server+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, pv.ErrEvidence
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, errors.New("application target API probe was incomplete")
	}
	defer func() { _ = response.Body.Close() }()
	content, err := io.ReadAll(io.LimitReader(response.Body, 64<<10+1))
	if err != nil || len(content) > 64<<10 {
		return 0, nil, errors.New("application API probe response exceeded its bound")
	}
	return response.StatusCode, content, nil
}
