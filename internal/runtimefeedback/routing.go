package runtimefeedback

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	MaxNodeURLsFileBytes = 64 << 10
	MaxNodeURLs          = 256
)

// endpoints freezes operator-owned routing at startup. Every lifecycle request
// selects only the node in its trusted workload binding, never an agent selector.
func (c Config) endpoints() (string, map[string]string, error) {
	if (c.URL == "") == (c.NodeURLsFile == "") {
		return "", nil, errors.New("runtime feedback requires exactly one HTTPS origin or node URLs file")
	}
	if c.URL != "" {
		endpoint, err := validateOrigin(c.URL)
		return endpoint, nil, err
	}
	endpoints, err := readNodeEndpoints(c.NodeURLsFile)
	return "", endpoints, err
}

func validateOrigin(origin string) (string, error) {
	endpoint, err := url.Parse(origin)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.RawPath != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return "", errors.New("runtime feedback requires an HTTPS service origin without user information, path, query, or fragment")
	}
	return strings.TrimRight(endpoint.String(), "/"), nil
}

func readNodeEndpoints(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("read runtime feedback node URLs file")
	}
	defer file.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(file, MaxNodeURLsFileBytes+1))
	if err != nil || len(data) > MaxNodeURLsFileBytes {
		return nil, errors.New("runtime feedback node URLs file exceeds bounds or cannot be read")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("runtime feedback node URLs file must be a JSON object")
	}
	endpoints := make(map[string]string)
	for decoder.More() {
		token, err = decoder.Token()
		node, ok := token.(string)
		if err != nil || !ok || len(validation.IsDNS1123Subdomain(node)) != 0 || len(endpoints) >= MaxNodeURLs {
			return nil, errors.New("runtime feedback node URLs contain an invalid node or exceed the entry limit")
		}
		if _, exists := endpoints[node]; exists {
			return nil, errors.New("runtime feedback node URLs contain a duplicate node")
		}
		var origin string
		if err := decoder.Decode(&origin); err != nil {
			return nil, errors.New("runtime feedback node URL must be an HTTPS origin string")
		}
		endpoint, err := validateOrigin(origin)
		if err != nil {
			return nil, err
		}
		endpoints[node] = endpoint
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || len(endpoints) == 0 {
		return nil, errors.New("runtime feedback node URLs file must contain at least one node")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("runtime feedback node URLs file has trailing data")
	}
	return endpoints, nil
}
