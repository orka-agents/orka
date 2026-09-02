package v2

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	RequestDigestDomain            = "orka.harness.v2.request.v1"
	ProfileDigestDomain            = "orka.harness.v2.profile.v1"
	AgentConfigurationDigestDomain = "orka.harness.v2.agent-configuration.v1"

	MaxCanonicalJSONBytes   = 2 << 20
	maxCanonicalJSONDepth   = 64
	maxCanonicalJSONItems   = 100_000
	maxCanonicalNumberChars = 8 * 1024
)

type canonicalNumber string

type canonicalParser struct {
	dec   *json.Decoder
	items int
}

// CanonicalJSON parses JSON with duplicate-key rejection and emits the stable
// v2 canonical form. Object keys are sorted by UTF-8 byte order, insignificant
// whitespace is removed, strings use encoding/json escaping, and numerically
// equivalent finite JSON numbers use the same non-exponent decimal spelling.
func CanonicalJSON(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("canonical JSON is empty")
	}
	if len(raw) > MaxCanonicalJSONBytes {
		return nil, fmt.Errorf("canonical JSON exceeds %d bytes", MaxCanonicalJSONBytes)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("canonical JSON contains invalid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	parser := canonicalParser{dec: dec}
	value, err := parser.parse(0)
	if err != nil {
		return nil, err
	}
	if tok, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("canonical JSON has trailing token %v", tok)
		}
		return nil, fmt.Errorf("canonical JSON has trailing data: %w", err)
	}
	var out bytes.Buffer
	if err := appendCanonicalJSON(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// CanonicalValue marshals a Go value and returns its canonical JSON form.
func CanonicalValue(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical value: %w", err)
	}
	return CanonicalJSON(raw)
}

// CanonicalRequestDigest hashes the canonical request after removing the sole
// metadata.requestDigest field. The domain separator and schema version prevent
// the digest from being reused as another kind of content digest.
func CanonicalRequestDigest(request any) (RequestDigest, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("marshal request for digest: %w", err)
	}
	return CanonicalRequestDigestJSON(raw)
}

// CanonicalRequestDigestJSON is the raw JSON variant of
// CanonicalRequestDigest. It rejects duplicate keys before digesting.
func CanonicalRequestDigestJSON(raw []byte) (RequestDigest, error) {
	value, err := parseCanonicalJSON(raw)
	if err != nil {
		return "", err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("request digest input must be a JSON object")
	}
	metadata, ok := root["metadata"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("request digest input requires metadata object")
	}
	digestValue, ok := metadata["requestDigest"]
	if !ok {
		return "", fmt.Errorf("request digest input requires metadata.requestDigest")
	}
	if _, ok := digestValue.(string); !ok {
		return "", fmt.Errorf("metadata.requestDigest must be a string")
	}
	delete(metadata, "requestDigest")
	// agentConfiguration was added after the v1 request-digest contract. Its
	// canonical digest is already bound by profile.agentConfigurationDigest, so
	// exclude the raw expansion to keep old/new controller-supervisor pairs
	// wire-compatible during a rolling upgrade.
	delete(root, "agentConfiguration")
	var canonical bytes.Buffer
	if err := appendCanonicalJSON(&canonical, root); err != nil {
		return "", err
	}
	return RequestDigest(hashCanonical(RequestDigestDomain, canonical.Bytes())), nil
}

// CanonicalProfileDigest returns a domain-separated digest for an immutable
// runtime profile.
func CanonicalProfileDigest(profile any) (ProfileDigest, error) {
	canonical, err := CanonicalValue(profile)
	if err != nil {
		return "", err
	}
	return ProfileDigest(hashCanonical(ProfileDigestDomain, canonical)), nil
}

// CanonicalAgentConfigurationDigest returns the domain-separated digest of one
// validated, resolved Agent session configuration.
func CanonicalLegacyAgentConfigurationDigest(configuration AgentSessionConfiguration, allowBash bool) (string, error) {
	if err := configuration.Validate(); err != nil {
		return "", err
	}
	if configuration.SystemPrompt != "" || configuration.ReasoningEffort != "" {
		return "", fmt.Errorf("legacy Agent configuration cannot bind system prompt or reasoning effort")
	}
	canonical, err := CanonicalValue(map[string]any{
		"agentUID": configuration.AgentUID, "agentGeneration": configuration.AgentGeneration,
		"runtime": configuration.ProviderKind, "model": configuration.Model,
		"maxTurns": configuration.MaxTurns, "allowBash": allowBash,
	})
	if err != nil {
		return "", err
	}
	return hashCanonical("orka.acp.agent-configuration", canonical), nil
}

func CanonicalAgentConfigurationDigest(configuration AgentSessionConfiguration) (string, error) {
	if err := configuration.Validate(); err != nil {
		return "", err
	}
	canonical, err := CanonicalValue(configuration)
	if err != nil {
		return "", err
	}
	return hashCanonical(AgentConfigurationDigestDomain, canonical), nil
}

func ValidateRequestDigest(digest RequestDigest) error {
	return validateSHA256Digest(string(digest))
}

func ValidateProfileDigest(digest ProfileDigest) error {
	return validateSHA256Digest(string(digest))
}

func hashCanonical(domain string, canonical []byte) string {
	h := sha256.New()
	_, _ = io.WriteString(h, domain)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(canonical)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func validateSHA256Digest(value string) error {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return fmt.Errorf("must be sha256:<64 lowercase hex characters>")
	}
	hexPart := strings.TrimPrefix(value, "sha256:")
	if strings.ToLower(hexPart) != hexPart {
		return fmt.Errorf("must use lowercase hexadecimal")
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return fmt.Errorf("invalid SHA-256 hexadecimal: %w", err)
	}
	return nil
}

func parseCanonicalJSON(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("canonical JSON is empty")
	}
	if len(raw) > MaxCanonicalJSONBytes {
		return nil, fmt.Errorf("canonical JSON exceeds %d bytes", MaxCanonicalJSONBytes)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("canonical JSON contains invalid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	parser := canonicalParser{dec: dec}
	value, err := parser.parse(0)
	if err != nil {
		return nil, err
	}
	if tok, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("canonical JSON has trailing token %v", tok)
		}
		return nil, fmt.Errorf("canonical JSON has trailing data: %w", err)
	}
	return value, nil
}

func (p *canonicalParser) parse(depth int) (any, error) {
	if depth > maxCanonicalJSONDepth {
		return nil, fmt.Errorf("canonical JSON exceeds maximum depth %d", maxCanonicalJSONDepth)
	}
	p.items++
	if p.items > maxCanonicalJSONItems {
		return nil, fmt.Errorf("canonical JSON exceeds maximum item count %d", maxCanonicalJSONItems)
	}
	tok, err := p.dec.Token()
	if err != nil {
		return nil, fmt.Errorf("parse canonical JSON: %w", err)
	}
	switch value := tok.(type) {
	case json.Delim:
		switch value {
		case '{':
			object := make(map[string]any)
			for p.dec.More() {
				keyToken, err := p.dec.Token()
				if err != nil {
					return nil, fmt.Errorf("parse canonical JSON object key: %w", err)
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, fmt.Errorf("canonical JSON object key is not a string")
				}
				if _, exists := object[key]; exists {
					return nil, fmt.Errorf("canonical JSON contains duplicate object key %q", key)
				}
				item, err := p.parse(depth + 1)
				if err != nil {
					return nil, err
				}
				object[key] = item
			}
			end, err := p.dec.Token()
			if err != nil || end != json.Delim('}') {
				return nil, fmt.Errorf("parse canonical JSON object terminator: %w", err)
			}
			return object, nil
		case '[':
			array := make([]any, 0)
			for p.dec.More() {
				item, err := p.parse(depth + 1)
				if err != nil {
					return nil, err
				}
				array = append(array, item)
			}
			end, err := p.dec.Token()
			if err != nil || end != json.Delim(']') {
				return nil, fmt.Errorf("parse canonical JSON array terminator: %w", err)
			}
			return array, nil
		default:
			return nil, fmt.Errorf("unexpected canonical JSON delimiter %q", value)
		}
	case json.Number:
		normalized, err := normalizeJSONNumber(value.String())
		if err != nil {
			return nil, err
		}
		return canonicalNumber(normalized), nil
	case string, bool, nil:
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported canonical JSON token %T", tok)
	}
}

func appendCanonicalJSON(out *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Errorf("encode canonical JSON string: %w", err)
		}
		out.Write(encoded)
	case canonicalNumber:
		out.WriteString(string(typed))
	case []any:
		out.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := appendCanonicalJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			encoded, err := json.Marshal(key)
			if err != nil {
				return fmt.Errorf("encode canonical JSON key: %w", err)
			}
			out.Write(encoded)
			out.WriteByte(':')
			if err := appendCanonicalJSON(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical JSON value %T", value)
	}
	return nil
}

func normalizeJSONNumber(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("canonical JSON number is empty")
	}
	sign := ""
	if strings.HasPrefix(raw, "-") {
		sign = "-"
		raw = strings.TrimPrefix(raw, "-")
	}
	exponent := 0
	if expIndex := strings.IndexAny(raw, "eE"); expIndex >= 0 {
		parsed, err := strconv.Atoi(raw[expIndex+1:])
		if err != nil {
			return "", fmt.Errorf("invalid canonical JSON number exponent %q", raw)
		}
		if parsed > maxCanonicalNumberChars || parsed < -maxCanonicalNumberChars {
			return "", fmt.Errorf("canonical JSON number exponent exceeds safe bound")
		}
		exponent = parsed
		raw = raw[:expIndex]
	}
	intPart, fracPart, _ := strings.Cut(raw, ".")
	if intPart == "" {
		return "", fmt.Errorf("invalid canonical JSON number %q", raw)
	}
	digitsRaw := intPart + fracPart
	leadingZeros := len(digitsRaw) - len(strings.TrimLeft(digitsRaw, "0"))
	digits := strings.TrimLeft(digitsRaw, "0")
	if digits == "" {
		return "0", nil
	}
	if len(digits) > maxCanonicalNumberChars {
		return "", fmt.Errorf("canonical JSON number exceeds safe length")
	}
	decimalPos := len(intPart) + exponent - leadingZeros
	var out string
	switch {
	case decimalPos <= 0:
		zeros := -decimalPos
		if zeros+len(digits)+2 > maxCanonicalNumberChars {
			return "", fmt.Errorf("canonical JSON number exceeds safe length")
		}
		out = "0." + strings.Repeat("0", zeros) + digits
	case decimalPos >= len(digits):
		zeros := decimalPos - len(digits)
		if len(digits)+zeros > maxCanonicalNumberChars {
			return "", fmt.Errorf("canonical JSON number exceeds safe length")
		}
		out = digits + strings.Repeat("0", zeros)
	default:
		out = digits[:decimalPos] + "." + digits[decimalPos:]
	}
	if dot := strings.IndexByte(out, '.'); dot >= 0 {
		whole := out[:dot]
		fraction := strings.TrimRight(out[dot+1:], "0")
		if fraction == "" {
			out = whole
		} else {
			out = whole + "." + fraction
		}
	}
	if strings.HasPrefix(out, "0.") {
		return sign + out, nil
	}
	out = strings.TrimLeft(out, "0")
	if out == "" {
		return "0", nil
	}
	return sign + out, nil
}

// CanonicalPromptSettlementDigest binds workspace validation to one exact
// terminal prompt settlement without reusing the request/profile digest domains.
func CanonicalPromptSettlementDigest(settlement PromptSettlement) (string, error) {
	if err := settlement.Validate(); err != nil {
		return "", err
	}
	canonical, err := CanonicalValue(settlement)
	if err != nil {
		return "", err
	}
	return hashCanonical("orka.harness.v2.prompt-settlement", canonical), nil
}
