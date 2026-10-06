package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

type NativeSessionExport struct {
	Data              []byte `json:"data"`
	DataDigest        string `json:"dataDigest"`
	ProviderSessionID string `json:"providerSessionID"`
}

type NativeSessionImportReceipt struct {
	Namespace         string `json:"namespace"`
	SessionName       string `json:"sessionName"`
	OperationID       string `json:"operationID"`
	RequestDigest     string `json:"requestDigest"`
	DataDigest        string `json:"dataDigest"`
	ProviderSessionID string `json:"providerSessionID"`
}

func (c *Client) nativeSessionRequest(ctx context.Context, method, name string, body []byte, response any) error {
	u, err := c.resourceURL("/api/v1/sessions/"+url.PathEscape(name)+"/native", nil)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("native session request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Keep portable conversation data out of error output.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("native session request failed (HTTP %d)", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("native session response exceeds transport bounds")
	}
	if err := json.Unmarshal(data, response); err != nil {
		return fmt.Errorf("invalid native session response")
	}
	return nil
}

func (c *Client) ExportNativeSession(ctx context.Context, name string) (*NativeSessionExport, error) {
	var response NativeSessionExport
	if err := c.nativeSessionRequest(ctx, http.MethodGet, name, nil, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (c *Client) ImportNativeSession(ctx context.Context, name, operationID string, data []byte) (*NativeSessionImportReceipt, error) {
	body, err := json.Marshal(struct {
		OperationID string `json:"operationID"`
		Data        []byte `json:"data"`
	}{operationID, data})
	if err != nil {
		return nil, err
	}
	var response NativeSessionImportReceipt
	if err := c.nativeSessionRequest(ctx, http.MethodPost, name, body, &response); err != nil {
		return nil, err
	}
	return &response, nil
}
