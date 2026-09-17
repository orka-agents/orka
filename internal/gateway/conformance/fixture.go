/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package conformance

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/orka-agents/orka/internal/gateway/protocol"
)

var (
	fixturePercentEscape = regexp.MustCompile(`%[0-9a-fA-F]{2}`)
	fixtureQuotedEscape  = regexp.MustCompile(`\\(?:u[dD][89aAbB][0-9a-fA-F]{2}\\u[dD][c-fC-F][0-9a-fA-F]{2}|u[0-9a-fA-F]{4}|U[0-9a-fA-F]{8}|x[0-9a-fA-F]{2}|[0-7]{3}|["\\/abfnrtv])`)
)

// DeliveryFixture supplies only the routing identities for a conformance delivery.
type DeliveryFixture struct {
	AccountID          string `json:"accountId"`
	ContextID          string `json:"contextId"`
	ThreadID           string `json:"threadId,omitempty"`
	ReplyTarget        string `json:"replyTarget"`
	OriginatingEventID string `json:"originatingEventId"`
}

type fixtureTransport struct{ http.RoundTripper }

func (t fixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	// An adapter can echo private routing in unsolicited HTTP/1.x bytes. Close
	// each connection so net/http cannot print those bytes from its idle reader.
	request = request.Clone(request.Context())
	request.Close = true
	return t.RoundTripper.RoundTrip(request)
}

func (f DeliveryFixture) deliveryRequest() (protocol.DeliveryRequest, error) {
	runID, err := uuid.NewRandom()
	if err != nil {
		return protocol.DeliveryRequest{}, errors.New("could not initialize delivery fixture")
	}
	id := "conformance-" + runID.String() + "-auth"
	delivery := protocol.DeliveryRequest{
		ProtocolVersion: protocol.Version, DeliveryID: id, IdempotencyID: id,
		Kind: protocol.DeliveryKindFinal, Text: "[Orka conformance check] No action required.",
		AccountID: f.AccountID, ContextID: f.ContextID, ThreadID: f.ThreadID,
		ReplyTarget: f.ReplyTarget, OriginatingEvent: f.OriginatingEventID,
	}
	if err := protocol.ValidateDeliveryRequest(&delivery); err != nil {
		return protocol.DeliveryRequest{}, errors.New("invalid delivery fixture")
	}
	// V1 validates trimmed required identities and does not bound the optional thread.
	// Check the original values because those are what the fixture transmits.
	for _, identity := range []string{f.AccountID, f.ContextID, f.ThreadID, f.ReplyTarget, f.OriginatingEventID} {
		if len(identity) > protocol.MaxIdentityBytes || !utf8.ValidString(identity) || strings.ContainsFunc(identity, unicode.IsControl) {
			return protocol.DeliveryRequest{}, errors.New("invalid delivery fixture")
		}
	}
	return delivery, nil
}

func (f DeliveryFixture) maskResultFields(
	message string, capabilities *protocol.CapabilitiesResponse,
) (string, *protocol.CapabilitiesResponse) {
	identities := []string{f.AccountID, f.ContextID, f.ThreadID, f.ReplyTarget, f.OriginatingEventID}
	unescape := func(value string) string {
		return fixturePercentEscape.ReplaceAllStringFunc(value, func(escape string) string {
			decoded, _ := url.PathUnescape(escape)
			return decoded
		})
	}
	unescapeQuoted := func(value string) string {
		return fixtureQuotedEscape.ReplaceAllStringFunc(value, func(escape string) string {
			var decoded string
			if err := json.Unmarshal([]byte(`"`+escape+`"`), &decoded); err == nil {
				return decoded
			}
			char, multibyte, tail, err := strconv.UnquoteChar(escape, '"')
			if err != nil || tail != "" {
				return escape
			}
			if !multibyte {
				return string([]byte{byte(char)})
			}
			return string(char)
		})
	}
	// Mask whole fields before bearer sanitization: partial replacements can hide
	// overlapping identities and expose their remaining fragments.
	mask := func(value string) string {
		candidates := []string{value}
		seen := map[string]bool{value: true}
		for index := 0; index < len(candidates); index++ {
			decoded := candidates[index]
			for _, identity := range identities {
				identity = strings.TrimSpace(identity)
				if identity == "" {
					continue
				}
				// HTTP and JSON errors can repeatedly quote an already quoted identity.
				// Escaping only grows the fragment, so the field length bounds this work.
				for escaped := identity; len(escaped) <= len(decoded); {
					if strings.Contains(decoded, escaped) {
						return redactedValue
					}
					quoted := strconv.Quote(escaped)
					next := quoted[1 : len(quoted)-1]
					if next == escaped {
						break
					}
					escaped = next
				}
			}
			// Check each decoding separately so an identity containing literal escape
			// text is not skipped when URL and string quoting are nested. Query spaces
			// use +; the path candidate preserves literal plus signs. Safe output and
			// transmitted routing stay unchanged.
			for _, next := range []string{unescape(decoded), unescape(strings.ReplaceAll(decoded, "+", " ")), unescapeQuoted(decoded)} {
				if seen[next] {
					continue
				}
				// Bound work on nested encodings without exposing an unchecked field.
				if len(candidates) == 8 {
					return redactedValue
				}
				seen[next] = true
				candidates = append(candidates, next)
			}
		}
		return value
	}
	message = mask(message)
	if capabilities == nil {
		return message, nil
	}
	result := *capabilities
	result.ProtocolVersion = mask(result.ProtocolVersion)
	result.AdapterName = mask(result.AdapterName)
	result.AdapterVersion = mask(result.AdapterVersion)
	return message, &result
}
