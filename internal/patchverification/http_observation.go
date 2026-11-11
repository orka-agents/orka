package patchverification

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	verificationv1 "github.com/orka-agents/orka/pkg/storage/v1/verification"
)

const (
	HTTPCheckVersion     = 1
	MaxHTTPResponseBytes = 4096
	maxHTTPPathBytes     = 2048
)

type HTTPCheck = verificationv1.HTTPCheck

type httpObservation struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// CheckExecutable selects the declared untrusted executable, never an observer.
// Runners must explicitly support HTTP checks; this is not an execution fallback.
func CheckExecutable(check Check) []string {
	if check.HTTP != nil {
		return check.HTTP.ServerCommand
	}
	return check.Command
}

// HTTPExpectation is the canonical status/body encoding shared by the frozen
// expectations and the trusted observer. Exit code zero belongs to the observer.
func HTTPExpectation(status int, body string) (Expectation, error) {
	if status < 200 || status > 599 || len(body) > MaxHTTPResponseBytes || !utf8.ValidString(body) {
		return Expectation{}, errors.New("HTTP observations require a final status and bounded UTF-8 body")
	}
	content, err := json.Marshal(httpObservation{Status: status, Body: body})
	if err != nil || len(content) > 8192 {
		return Expectation{}, errors.New("canonical HTTP observation exceeds the expectation limit")
	}
	return Expectation{Stdout: string(content)}, nil
}

// MatchedHTTPOutput reconstructs only already-frozen bytes whose digest, length,
// and exit code match a usable protected observation. Unknown output stays absent.
func MatchedHTTPOutput(check Check, observation Observation) (string, bool) {
	if check.ID != observation.CheckID || ValidateHTTPCheck(check) != nil || !usableCheck(check, observation) {
		return "", false
	}
	for _, expectation := range []Expectation{check.Healthy, check.Failure} {
		if matches(observation, expectation) {
			return expectation.Stdout, true
		}
	}
	return "", false
}

func validateHTTPExpectation(expectation Expectation) error {
	var observation httpObservation
	decoder := json.NewDecoder(strings.NewReader(expectation.Stdout))
	decoder.DisallowUnknownFields()
	if expectation.ExitCode != 0 || len(expectation.Services) != 0 || len(expectation.Stdout) > 8192 ||
		decoder.Decode(&observation) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("HTTP expectations must contain only a canonical status and body with observer exit zero")
	}
	canonical, err := HTTPExpectation(observation.Status, observation.Body)
	if err != nil || canonical.Stdout != expectation.Stdout {
		return errors.New("HTTP expectation is not the canonical bounded status/body encoding")
	}
	return nil
}

// ValidateHTTPCheck rejects executable or in-process oracles. It does not
// establish that the declared external observations adequately test a report.
func ValidateHTTPCheck(check Check) error {
	if check.HTTP == nil || check.HTTP.Version != HTTPCheckVersion {
		return errors.New("a supported protected HTTP observation contract is required")
	}
	if len(check.Command) != 0 || check.Stdin != "" || len(check.Lifecycle) != 0 {
		return errors.New("protected HTTP observations do not support raw checks, stdin, or lifecycle interactions")
	}
	command := CheckExecutable(check)
	if len(command) == 0 || len(command) > 128 || !strings.HasPrefix(command[0], "/checks/") ||
		!sourceValidPath(strings.TrimPrefix(command[0], "/checks/")) {
		return errors.New("HTTP server command must name a frozen executable under /checks")
	}
	for _, argument := range command {
		if len(argument) > 8192 || strings.ContainsRune(argument, 0) || !utf8.ValidString(argument) {
			return errors.New("HTTP server command contains an invalid argument")
		}
	}
	if err := validateHTTPPath(check.HTTP.Path); err != nil {
		return err
	}
	if check.TimeoutSeconds < 1 || check.TimeoutSeconds > 300 {
		return errors.New("HTTP observation requires a timeout between 1 and 300 seconds")
	}
	for _, expectation := range []Expectation{check.Healthy, check.Failure} {
		if err := validateHTTPExpectation(expectation); err != nil {
			return err
		}
	}
	if check.Healthy.Stdout == check.Failure.Stdout {
		return errors.New("HTTP healthy and failure observations must differ")
	}
	return nil
}

func validateHTTPPath(path string) error {
	parsed, err := url.ParseRequestURI(path)
	if err != nil || len(path) > maxHTTPPathBytes || !utf8.ValidString(path) || !strings.HasPrefix(path, "/") ||
		strings.HasPrefix(path, "//") || strings.ContainsAny(path, "#\\\r\n\x00") ||
		parsed.Scheme != "" || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.Fragment != "" || parsed.RequestURI() != path {
		return errors.New("HTTP observation requires a bounded relative request path without an authority or fragment")
	}
	return nil
}

func validateHTTPManifest(manifest Manifest) error {
	protected := false
	for _, check := range manifest.Checks {
		protected = protected || check.HTTP != nil
	}
	if !protected {
		return nil
	}
	environment := manifest.Environment
	if environment.Profile != LocalServices || len(environment.Services) != 0 {
		return errors.New("protected HTTP observations require local-services with only the implicit subject server")
	}
	if environment.Dependencies["orka.kubernetes.policy"] != KubernetesPolicyVersion ||
		environment.Dependencies["orka.local-runner.policy"] != "" {
		return errors.New("protected HTTP observations require the HTTP-capable Kubernetes worker; Docker is unsupported")
	}
	if len(MissingRequirements(environment)) != 0 {
		return errors.New("protected HTTP observations do not provide outbound services, clusters, or additional identities")
	}
	for _, check := range manifest.Checks {
		if err := ValidateHTTPCheck(check); err != nil {
			return err
		}
	}
	return nil
}

// ObserveHTTP connects only to the supervisor-assigned IPv6 loopback listener.
// Returned bytes are a complete observation, never subject stdout or a verdict.
func ObserveHTTP(ctx context.Context, check Check, port int) ([]byte, error) {
	if err := ValidateHTTPCheck(check); err != nil {
		return nil, err
	}
	if port < 1024 || port > 65535 {
		return nil, errors.New("HTTP observation requires an assigned unprivileged listener")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(check.TimeoutSeconds)*time.Second)
	defer cancel()
	endpoint := net.JoinHostPort("::1", strconv.Itoa(port))
	transport := &http.Transport{
		Proxy: nil, DisableCompression: true, DisableKeepAlives: true,
		MaxResponseHeaderBytes: 8192, MaxConnsPerHost: 1,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != endpoint {
				return nil, errors.New("HTTP observer cannot dial a different endpoint")
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp6", endpoint)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+endpoint+check.HTTP.Path, nil)
	if err != nil {
		return nil, errors.New("HTTP observation request could not be constructed")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("HTTP observation did not receive a complete response")
	}
	content, readErr := io.ReadAll(io.LimitReader(response.Body, MaxHTTPResponseBytes+1))
	closeErr := response.Body.Close()
	if ctx.Err() != nil || readErr != nil || closeErr != nil {
		return nil, errors.New("HTTP observation was canceled, late, or incomplete")
	}
	expectation, err := HTTPExpectation(response.StatusCode, string(content))
	if err != nil {
		return nil, err
	}
	return []byte(expectation.Stdout), nil
}
