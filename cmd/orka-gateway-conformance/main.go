/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/gateway/conformance"
	"github.com/orka-agents/orka/internal/gateway/protocol"
)

func main() {
	endpoint := flag.String("endpoint", "", "adapter base URL")
	tokenEnv := flag.String(
		"token-env", "ORKA_GATEWAY_BEARER_TOKEN",
		"environment variable containing the outbound bearer token",
	)
	timeout := flag.Duration("timeout", 15*time.Second, "per-request timeout")
	referenceFixtures := flag.Bool("reference-fixtures", false, "run optional reference-adapter fault fixtures")
	deliveryFixturePath := flag.String(
		"delivery-fixture", "", "JSON file containing delivery routing identities (sends a real test message)",
	)
	flag.Parse()
	if strings.TrimSpace(*endpoint) == "" {
		fmt.Fprintln(os.Stderr, "--endpoint is required")
		os.Exit(2)
	}
	token := strings.TrimSpace(os.Getenv(*tokenEnv))
	if token == "" {
		fmt.Fprintf(os.Stderr, "%s is required\n", *tokenEnv)
		os.Exit(2)
	}
	var deliveryFixture *conformance.DeliveryFixture
	if *deliveryFixturePath != "" {
		if *referenceFixtures {
			fmt.Fprintln(os.Stderr, "delivery fixture cannot be combined with reference fixtures")
			os.Exit(2)
		}
		var err error
		deliveryFixture, err = loadDeliveryFixture(*deliveryFixturePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4**timeout)
	defer cancel()
	result := conformance.Check(ctx, conformance.Target{
		BaseURL: *endpoint, AuthorizationValue: token, Timeout: *timeout, ReferenceFixtures: *referenceFixtures,
		DeliveryFixture: deliveryFixture,
	})
	_ = writeResult(os.Stdout, result, token)
	if !result.Passed {
		os.Exit(1)
	}
}

func loadDeliveryFixture(path string) (*conformance.DeliveryFixture, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("could not read delivery fixture")
	}
	defer file.Close() //nolint:errcheck
	body, err := io.ReadAll(io.LimitReader(file, protocol.MaxHTTPBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("could not read delivery fixture")
	}
	if len(body) > protocol.MaxHTTPBodyBytes || !validFixtureUnicode(body) || !uniqueFixtureKeys(body) {
		return nil, fmt.Errorf("invalid delivery fixture")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var fixture *conformance.DeliveryFixture
	if err := decoder.Decode(&fixture); err != nil || fixture == nil {
		return nil, fmt.Errorf("invalid delivery fixture")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("invalid delivery fixture")
	}
	return fixture, nil
}

// encoding/json accepts repeated keys and matches these ASCII field names
// case-insensitively. Reject aliases before a later value can replace an identity.
func uniqueFixtureKeys(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		key, ok := token.(string)
		if !ok {
			return false
		}
		key = strings.ToLower(key)
		if seen[key] {
			return false
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return false
		}
	}
	closing, err := decoder.Token()
	return err == nil && closing == json.Delim('}')
}

// encoding/json repairs invalid UTF-8 and unpaired UTF-16 surrogates.
// Reject both before decoding can change a routing identity.
func validFixtureUnicode(body []byte) bool {
	if !utf8.Valid(body) {
		return false
	}
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		i++
		if i >= len(body) || body[i] != 'u' {
			continue
		}
		if i+4 >= len(body) {
			return false
		}
		first, err := strconv.ParseUint(string(body[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if !utf16.IsSurrogate(rune(first)) {
			continue
		}
		if i+6 >= len(body) || body[i+1] != '\\' || body[i+2] != 'u' {
			return false
		}
		second, err := strconv.ParseUint(string(body[i+3:i+7]), 16, 16)
		if err != nil || utf16.DecodeRune(rune(first), rune(second)) == utf8.RuneError {
			return false
		}
		i += 6
	}
	return true
}

func writeResult(writer io.Writer, result conformance.CheckResult, token string) error {
	return json.NewEncoder(writer).Encode(conformance.SanitizeCheckResult(result, token))
}
