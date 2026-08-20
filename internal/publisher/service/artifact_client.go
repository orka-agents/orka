package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/orka-agents/orka/internal/artifactcap"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const maxArtifactResponseBytes = int64(64 << 10)

type artifactClient struct {
	baseURL    *url.URL
	client     *http.Client
	authorizer artifactAuthorizer
}

func newArtifactClient(rawURL string, client *http.Client, authorizer artifactAuthorizer, timeout time.Duration) (*artifactClient, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("artifact API URL must be an absolute credential-free HTTP(S) URL")
	}
	if parsed.Scheme != schemeHTTP && parsed.Scheme != schemeHTTPS || parsed.Path != "" && parsed.Path != "/" {
		return nil, fmt.Errorf("artifact API URL must contain only scheme and authority")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	if client == nil {
		// Authenticated in-cluster controller traffic must never traverse an
		// inherited environment proxy.
		client = &http.Client{Timeout: timeout, Transport: harnessv2.NewProxylessTransport()}
	} else {
		clone := *client
		client = &clone
		if client.Timeout == 0 {
			client.Timeout = timeout
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if authorizer == nil {
		return nil, fmt.Errorf("artifact authorizer is required")
	}
	return &artifactClient{baseURL: parsed, client: client, authorizer: authorizer}, nil
}

func (c *artifactClient) upload(
	ctx context.Context,
	parent Operation,
	metadata OperationMetadata,
	attempt int,
	reference harnessv2.ArtifactReference,
	body io.Reader,
) error {
	if err := validateArtifactTransportReference(reference); err != nil {
		return err
	}
	authorization, err := c.authorizer.Authorize(ctx, ArtifactAuthorizationRequest{
		ParentOperation: parent, Metadata: metadata, ArtifactOperation: artifactcap.OperationUpload,
		Artifact: reference, Attempt: attempt,
	})
	if err != nil {
		return apiError(ErrArtifactTransport, "artifact_authorization_failed", "artifact upload could not be authorized", 503, operationErrorIsRetryable(err), err)
	}
	request, err := c.request(ctx, http.MethodPut, reference, authorization, body)
	if err != nil {
		return err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return apiError(ErrArtifactTransport, "artifact_transport_failed", "artifact upload transport failed", 503, true, err)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxArtifactResponseBytes))
		retryable := response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests
		return apiError(ErrArtifactTransport, "artifact_upload_failed", "artifact API rejected the upload", response.StatusCode, retryable, nil)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxArtifactResponseBytes+1))
	if err != nil || int64(len(data)) > maxArtifactResponseBytes {
		return apiError(ErrArtifactTransport, "artifact_response_invalid", "artifact API returned an invalid response", 502, true, err)
	}
	var stored artifactcap.Artifact
	if err := decodeStrict(data, &stored); err != nil || stored.Validate() != nil {
		return apiError(ErrArtifactTransport, "artifact_response_invalid", "artifact API returned an invalid response", 502, false, err)
	}
	actual := harnessv2.ArtifactReference{ArtifactID: harnessv2.ArtifactID(stored.ArtifactID), Digest: stored.Digest, SizeBytes: stored.SizeBytes, MediaType: stored.MediaType}
	if actual != reference {
		return apiError(ErrArtifactTransport, "artifact_response_mismatch", "artifact API response did not match the requested object", 502, false, nil)
	}
	return nil
}

func (c *artifactClient) download(ctx context.Context, parent Operation, metadata OperationMetadata, attempt int, reference harnessv2.ArtifactReference, limit int64) ([]byte, error) {
	if err := validateArtifactTransportReference(reference); err != nil {
		return nil, err
	}
	if reference.SizeBytes > limit {
		return nil, invalidRequest("artifact exceeds the configured download limit", nil)
	}
	authorization, err := c.authorizer.Authorize(ctx, ArtifactAuthorizationRequest{
		ParentOperation: parent, Metadata: metadata, ArtifactOperation: artifactcap.OperationDownload,
		Artifact: reference, Attempt: attempt,
	})
	if err != nil {
		return nil, apiError(ErrArtifactTransport, "artifact_authorization_failed", "artifact download could not be authorized", 503, operationErrorIsRetryable(err), err)
	}
	request, err := c.request(ctx, http.MethodGet, reference, authorization, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, apiError(ErrArtifactTransport, "artifact_transport_failed", "artifact download transport failed", 503, true, err)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxArtifactResponseBytes))
		retryable := response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests
		return nil, apiError(ErrArtifactTransport, "artifact_download_failed", "artifact API rejected the download", response.StatusCode, retryable, nil)
	}
	if response.ContentLength != reference.SizeBytes || response.Header.Get("Content-Type") != reference.MediaType ||
		response.Header.Get(artifactcap.ObjectDigestHeader) != reference.Digest {
		return nil, apiError(ErrArtifactTransport, "artifact_response_mismatch", "artifact download metadata did not match the requested object", 502, false, nil)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) != reference.SizeBytes || int64(len(data)) > limit {
		return nil, apiError(ErrArtifactTransport, "artifact_response_invalid", "artifact download body was incomplete or oversized", 502, true, err)
	}
	if !constantEqual(artifactcap.DigestBytes(data), reference.Digest) {
		return nil, apiError(ErrArtifactTransport, "artifact_digest_mismatch", "artifact download digest did not match", 502, false, nil)
	}
	return data, nil
}

func (c *artifactClient) request(ctx context.Context, method string, reference harnessv2.ArtifactReference, authorization artifactcap.Authorization, body io.Reader) (*http.Request, error) {
	objectPath, err := artifactcap.ObjectPath(reference.Digest)
	if err != nil {
		return nil, invalidRequest("artifact digest is invalid", err)
	}
	endpoint := *c.baseURL
	endpoint.Path = objectPath
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, apiError(ErrArtifactTransport, "artifact_request_invalid", "artifact request could not be created", 500, false, err)
	}
	request.Header.Set(artifactcap.CapabilityHeader, authorization.Capability)
	request.Header.Set(artifactcap.RequestDigestHeader, authorization.RequestDigest)
	request.Header.Set(artifactcap.ContentLengthHeader, strconv.FormatInt(reference.SizeBytes, 10))
	request.Header.Set(artifactcap.MediaTypeHeader, reference.MediaType)
	if method == http.MethodPut {
		request.ContentLength = reference.SizeBytes
		request.Header.Set("Content-Type", reference.MediaType)
	} else {
		request.Header.Set("Accept", reference.MediaType)
	}
	return request, nil
}

func validateArtifactTransportReference(reference harnessv2.ArtifactReference) error {
	if err := reference.Validate(); err != nil {
		return invalidRequest("artifact reference is invalid", err)
	}
	expectedID, err := artifactcap.ArtifactIDForDigest(reference.Digest)
	if err != nil || !constantEqual(string(reference.ArtifactID), expectedID) || reference.SizeBytes < 0 {
		return invalidRequest("artifact reference is not exactly content addressed", err)
	}
	return nil
}

func encodeJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
