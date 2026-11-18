package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/publisher"
)

const (
	repairPatchMaxCapabilityBytes = 32 << 10
	repairPatchRequestTimeout     = 2 * time.Minute
)

var (
	// The capability Secret is refreshed while the download has not started;
	// a 403 means the mounted copy is stale or not yet propagated.
	repairPatchRetryInterval = 5 * time.Second
	repairPatchRetryWindow   = 4 * time.Minute
)

// repairPatchCapability mirrors tools.RepositoryRepairValidationCapability.
type repairPatchCapability struct {
	Capability    string             `json:"capability"`
	RequestDigest string             `json:"requestDigest"`
	Request       repairPatchRequest `json:"request"`
}

type repairPatchRequest struct {
	Operation     string `json:"operation"`
	ObjectDigest  string `json:"objectDigest"`
	Namespace     string `json:"namespace"`
	TaskID        string `json:"taskID"`
	ContentLength int64  `json:"contentLength"`
	MediaType     string `json:"mediaType"`
	OperationID   string `json:"operationID"`
}

// applyRepositoryRepairPatch downloads one repair patch with its single-use
// capability and applies it to the validation checkout. Download problems map
// to the validation-unavailable exit code; a rejected patch is a plain failure.
func applyRepositoryRepairPatch(ctx context.Context, args []string) error {
	if len(args) != 6 {
		return fmt.Errorf("%w: usage: --apply-repository-repair-patch <controllerURL> <capabilityFile> "+
			"<repositoryRoot> <expectedDigest> <relativeRoot> <denyControlPaths>", errValidationCommandUnavailable)
	}
	controllerURL, capabilityFile, repositoryRoot := args[0], args[1], args[2]
	expectedDigest, relativeRoot := args[3], args[4]
	deny, err := strconv.ParseBool(args[5])
	if err != nil || args[5] != strconv.FormatBool(deny) {
		return fmt.Errorf("%w: denyControlPaths must be true or false", errValidationCommandUnavailable)
	}
	if _, err := artifactcap.DigestHex(expectedDigest); err != nil {
		return fmt.Errorf("%w: expected patch digest is invalid", errValidationCommandUnavailable)
	}
	artifact, err := downloadRepairPatch(ctx, controllerURL, capabilityFile, expectedDigest)
	if err != nil {
		return err
	}
	err = publisher.ApplyDeltaToDirectory(ctx, repositoryRoot, artifact, expectedDigest, relativeRoot, deny)
	if err != nil {
		return fmt.Errorf("repair patch rejected: %w", err)
	}
	fmt.Fprintln(os.Stderr, "repair patch applied")
	return nil
}

func readRepairPatchCapability(capabilityFile, expectedDigest string) (repairPatchCapability, error) {
	file, err := os.Open(capabilityFile)
	if err != nil {
		return repairPatchCapability{}, fmt.Errorf("%w: read patch capability", errValidationCommandUnavailable)
	}
	defer file.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(file, repairPatchMaxCapabilityBytes+1))
	if err != nil || len(data) > repairPatchMaxCapabilityBytes {
		return repairPatchCapability{}, fmt.Errorf("%w: read patch capability", errValidationCommandUnavailable)
	}
	var document repairPatchCapability
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return repairPatchCapability{}, fmt.Errorf("%w: decode patch capability", errValidationCommandUnavailable)
	}
	request := document.Request
	if document.Capability == "" || !artifactcap.IsRequestDigest(document.RequestDigest) ||
		request.Operation != string(artifactcap.OperationDownload) || request.ObjectDigest != expectedDigest ||
		request.MediaType != artifactcap.MediaTypeWorkspaceDelta ||
		request.ContentLength <= 0 || request.ContentLength > artifactcap.DefaultWorkspaceArtifactMaxBytes {
		return repairPatchCapability{}, fmt.Errorf(
			"%w: patch capability does not authorize this patch download", errValidationCommandUnavailable)
	}
	return document, nil
}

func downloadRepairPatch(ctx context.Context, controllerURL, capabilityFile, expectedDigest string) ([]byte, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(controllerURL), "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("%w: controller URL is invalid", errValidationCommandUnavailable)
	}
	objectPath, err := artifactcap.ObjectPath(expectedDigest)
	if err != nil {
		return nil, fmt.Errorf("%w: patch digest is invalid", errValidationCommandUnavailable)
	}
	endpoint := base.String() + objectPath
	client := &http.Client{
		Timeout: repairPatchRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	deadline := time.Now().Add(repairPatchRetryWindow)
	for {
		document, err := readRepairPatchCapability(capabilityFile, expectedDigest)
		if err != nil {
			return nil, err
		}
		artifact, status, err := fetchRepairPatch(ctx, client, endpoint, document)
		if err == nil {
			return verifyRepairPatch(artifact, document.Request.ContentLength, expectedDigest)
		}
		if status != http.StatusForbidden || !time.Now().Add(repairPatchRetryInterval).Before(deadline) {
			return nil, err
		}
		fmt.Fprintln(os.Stderr, "repair patch capability was not accepted; waiting for a refreshed capability")
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %v", errValidationCommandUnavailable, ctx.Err())
		case <-time.After(repairPatchRetryInterval):
		}
	}
}

func fetchRepairPatch(
	ctx context.Context, client *http.Client, endpoint string, document repairPatchCapability,
) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: build patch request", errValidationCommandUnavailable)
	}
	request.Header.Set(artifactcap.CapabilityHeader, document.Capability)
	request.Header.Set(artifactcap.RequestDigestHeader, document.RequestDigest)
	request.Header.Set(artifactcap.ContentLengthHeader, strconv.FormatInt(document.Request.ContentLength, 10))
	request.Header.Set(artifactcap.MediaTypeHeader, document.Request.MediaType)
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: download repair patch: transport failure", errValidationCommandUnavailable)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return nil, response.StatusCode, fmt.Errorf(
			"%w: download repair patch: HTTP %d", errValidationCommandUnavailable, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, document.Request.ContentLength+1))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf(
			"%w: download repair patch: read failure", errValidationCommandUnavailable)
	}
	return data, response.StatusCode, nil
}

// verifyRepairPatch rejects a patch whose bytes differ from the bound digest
// before any of it is applied. A mismatch is a patch failure, not an outage.
func verifyRepairPatch(artifact []byte, contentLength int64, expectedDigest string) ([]byte, error) {
	if int64(len(artifact)) != contentLength {
		return nil, errors.New("repair patch length does not match its bound request")
	}
	sum := sha256.Sum256(artifact)
	if "sha256:"+hex.EncodeToString(sum[:]) != expectedDigest {
		return nil, errors.New("repair patch digest does not match its bound request")
	}
	return artifact, nil
}
