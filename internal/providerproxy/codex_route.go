package providerproxy

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// These fields exist only between an authenticated supervisor and the Orka
// auth proxy. Neither the child nor the model gateway may supply or see them.
const (
	Codex429RouteHeader = "X-Orka-Codex-429-Route"
	Codex429RetryHeader = "X-Orka-Codex-429-Retry"
	// Older auth proxies ignore the precondition header but cannot authorize
	// this scheme. A mixed-version rollout must never forward an unchecked retry.
	Codex429RetryAuthScheme = "Orka-Codex-429"
)

var (
	qualifiedFrontend = regexp.MustCompile(`^codex-429-([a-f0-9]{32})$`)
	qualifiedBackend  = regexp.MustCompile(`-[a-f0-9]{6,}$`)
	qualifiedDNSName  = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	errQualification  = errors.New("invalid Codex 429 qualification: expected an exact versioned URL and model")
)

func qualifiedURL(raw, path, model string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
		u.Path != path || u.RawPath != "" || strings.ContainsAny(raw, "?#") || raw != u.String() ||
		!qualifiedDNSName.MatchString(u.Hostname()) || len(u.Hostname()) > 253 ||
		model == "" || strings.ContainsAny(model, "*?") || strings.ContainsFunc(model, func(r rune) bool { return !unicode.IsPrint(r) || unicode.IsSpace(r) }) {
		return nil, errQualification
	}
	if u.Host != u.Hostname() {
		n, err := strconv.ParseUint(u.Port(), 10, 16)
		if err != nil || n == 0 || u.Host != u.Hostname()+":"+strconv.FormatUint(n, 10) {
			return nil, errQualification
		}
	}
	return u, nil
}

// Codex429FrontendID checks the supervisor's independent operator pin against
// the actual first hop and frozen model. A well-formed pin for a different
// pool does not prevent that pool from serving requests without retries.
func Codex429FrontendID(actualURL, model, pinURL, pinModel string) (string, error) {
	if pinURL == "" && pinModel == "" {
		return "", nil
	}
	u, err := qualifiedURL(pinURL, "/v1", pinModel)
	if err != nil {
		return "", err
	}
	label, _, _ := strings.Cut(u.Hostname(), ".")
	match := qualifiedFrontend.FindStringSubmatch(label)
	if match == nil {
		return "", errQualification
	}
	if actualURL != pinURL || model != pinModel {
		return "", nil
	}
	return match[1], nil
}

// Codex429EgressID binds the auth proxy to the gateway origin it actually
// dials, not merely the name of the Service in front of this proxy.
func Codex429EgressID(actual *url.URL, pinURL, pinModel string) (string, error) {
	if pinURL == "" && pinModel == "" {
		return "", nil
	}
	u, err := qualifiedURL(pinURL, "", pinModel)
	if err != nil {
		return "", err
	}
	label, _, _ := strings.Cut(u.Hostname(), ".")
	if !qualifiedBackend.MatchString(label) {
		return "", errQualification
	}
	if actual.Path != "/" || actual.RawPath != "" || actual.ForceQuery || actual.String() != pinURL+"/" {
		return "", nil
	}
	sum := sha256.Sum256([]byte("orka-codex-429-v1\n" + pinURL + "\n" + pinModel + "\n"))
	return hex.EncodeToString(sum[:16]), nil
}

// NewCodex429Challenge makes an upstream-supplied or stale response header
// useless even when an older auth proxy strips requests but not responses.
func NewCodex429Challenge(id string) string {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ""
	}
	return id + "." + hex.EncodeToString(nonce[:])
}

// Codex429Challenge accepts a single non-list value for this exact route.
func Codex429Challenge(h http.Header, id string) string {
	values := h.Values(Codex429RouteHeader)
	if id == "" || len(values) != 1 {
		return ""
	}
	nonce, ok := strings.CutPrefix(values[0], id+".")
	if !ok || len(nonce) != 32 || strings.Trim(nonce, "0123456789abcdef") != "" {
		return ""
	}
	return values[0]
}

type discardJSON struct{}

func (*discardJSON) UnmarshalJSON([]byte) error { return nil }

// DirectCodexHeaders excludes stateful, upgraded and rerouted operations.
// The two Orka protocol headers are filtered and generated separately.
func DirectCodexHeaders(h http.Header) bool {
	for key := range h {
		name := http.CanonicalHeaderKey(key)
		switch name {
		case "Upgrade", "X-Codex-Turn-State", "X-Http-Method-Override", "X-Http-Method", "X-Method-Override",
			"X-Original-Method", "X-Forwarded-Method", "X-Original-Url", "X-Rewrite-Url", "X-Forwarded-Host",
			"X-Forwarded-Prefix", "X-Forwarded-Uri", "X-Envoy-Original-Path":
			return false
		}
		if strings.HasPrefix(name, "X-Vekil-") || strings.HasPrefix(name, "Sec-Websocket-") {
			return false
		}
	}
	for _, line := range h.Values("Connection") {
		for part := range strings.SplitSeq(line, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "upgrade") {
				return false
			}
		}
	}
	return true
}

// DirectCodexBody accepts an unambiguous streaming Responses selector, not
// merely JSON that unmarshals into the frozen model. Do not search opaque
// prompt text, tool arguments, schemas or tool results for selector names.
func DirectCodexBody(body []byte, want string) bool {
	d := json.NewDecoder(bytes.NewReader(body))
	seen := make(map[string]bool)
	var discard discardJSON
	ok := codexObject(d, body, func(key string, literal bool) bool {
		for _, selector := range [...]string{"model", "stream", "input", "background", "store", "conversation", "previous_response_id", "context_management"} {
			if !strings.EqualFold(key, selector) {
				continue
			}
			if !literal || key != selector || seen[key] {
				return false
			}
			seen[key] = true
			switch key {
			case "model":
				return codexToken(d, want)
			case "stream":
				return codexToken(d, true)
			case "input":
				return directCodexInput(d, body)
			case "background", "store":
				return codexToken(d, false)
			default: // server-side conversation or compaction
				return false
			}
		}
		return d.Decode(&discard) == nil
	})
	return ok && seen["model"] && seen["stream"] && d.Decode(&discard) == io.EOF
}

func codexToken(d *json.Decoder, want json.Token) bool {
	tok, err := d.Token()
	return err == nil && tok == want
}

func codexObject(d *json.Decoder, body []byte, field func(string, bool) bool) bool {
	if !codexToken(d, json.Delim('{')) {
		return false
	}
	for d.More() {
		start := d.InputOffset()
		tok, err := d.Token()
		key, ok := tok.(string)
		// Token unescapes names. Inspect only the just-consumed key, never
		// the following value; intervening commas and whitespace contain no escapes.
		if err != nil || !ok || !field(key, bytes.IndexByte(body[start:d.InputOffset()], '\\') == -1) {
			return false
		}
	}
	return codexToken(d, json.Delim('}'))
}

func directCodexInput(d *json.Decoder, body []byte) bool {
	tok, err := d.Token()
	if err != nil {
		return false
	}
	if _, ok := tok.(string); ok {
		return true
	}
	if tok != json.Delim('[') {
		return false
	}
	for d.More() {
		seenType := false
		if !codexObject(d, body, func(key string, literal bool) bool {
			if !strings.EqualFold(key, "type") {
				return d.Decode(&discardJSON{}) == nil
			}
			if !literal || seenType || key != "type" {
				return false
			}
			seenType = true
			var typ string
			if d.Decode(&typ) != nil || typ == "" {
				return false
			}
			switch strings.ToLower(strings.TrimSpace(typ)) {
			case "compaction_trigger", "compaction", "context_compaction", "item_reference":
				return false
			}
			return true
		}) {
			return false
		}
	}
	return codexToken(d, json.Delim(']'))
}
