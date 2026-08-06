package security

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strconv"
	"strings"
	"unicode"

	"github.com/orka-agents/orka/internal/store"
)

const SemanticIdentityAlgorithmV1 = "semantic-v1"

type SemanticIdentity struct {
	RuleID              string
	Anchor              string
	Instance            string
	Quality             string
	AlgorithmVersion    string
	SemanticFingerprint string
	SemanticFindingID   string
}

func semanticSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			out.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && out.Len() > 0 {
				out.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(out.String(), "-")
}

func fullDigest(domain string, parts ...string) string {
	input := append([]string{domain}, parts...)
	digest := sha256.Sum256([]byte(strings.Join(input, "\x00")))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func digestID(prefix, digest string) string {
	return prefix + strings.TrimPrefix(digest, "sha256:")
}

// DeriveSemanticIdentity creates an additive full-width identity from controller-normalized finding data.
func DeriveSemanticIdentity(repoURL string, finding *store.Finding) SemanticIdentity {
	return deriveSemanticIdentity(repoURL, FindingsV2Finding{}, finding)
}

// DeriveSemanticIdentityForCandidate preserves bounded producer identity proposals while keeping their quality non-authoritative.
func DeriveSemanticIdentityForCandidate(repoURL string, candidate FindingsV2Finding, finding *store.Finding) SemanticIdentity {
	return deriveSemanticIdentity(repoURL, candidate, finding)
}

// Until a versioned rule policy independently validates producer proposals, quality is
// producer-proposed and must not drive automatic resolution or suppression.
func deriveSemanticIdentity(repoURL string, candidate FindingsV2Finding, finding *store.Finding) SemanticIdentity {
	repositoryCoordinate := canonicalRepositoryURLCoordinate(repoURL)
	ruleID := semanticSlug(candidate.RuleID)
	if ruleID == "" {
		ruleID = "security-finding"
	}
	anchor := ""
	instance := ""
	if candidate.Identity != nil {
		anchor = semanticSlug(candidate.Identity.Anchor)
		instance = semanticSlug(candidate.Identity.Instance)
	}

	legacyFingerprint := ""
	if finding != nil {
		if ruleID == "security-finding" {
			if slug := semanticSlug(finding.Category); slug != "" {
				ruleID = slug
			}
		}
		legacyFingerprint = finding.Fingerprint
		if anchor == "" && len(finding.Evidence) > 0 {
			ref := finding.Evidence[0]
			root := strings.TrimSpace(ref.Symbol)
			if root == "" {
				root = strings.TrimSuffix(path.Clean(strings.ReplaceAll(ref.Path, "\\", "/")), path.Ext(ref.Path))
			}
			anchorDigest := fullDigest("semantic-anchor-v1", repositoryCoordinate, root)
			anchor = "control-" + strings.TrimPrefix(anchorDigest, "sha256:")[:20]
		}
	}
	if anchor == "" {
		anchor = "unknown-control"
	}
	if instance == "" && legacyFingerprint != "" {
		legacyDigest := fullDigest("semantic-instance-fallback-v1", legacyFingerprint)
		instance = "legacy-" + strings.TrimPrefix(legacyDigest, "sha256:")[:20]
	}
	fingerprint := semanticFingerprintForRepositoryIdentity(repositoryCoordinate, ruleID, anchor, instance)
	return SemanticIdentity{
		RuleID:              ruleID,
		Anchor:              anchor,
		Instance:            instance,
		Quality:             store.IdentityQualityProducerProposed,
		AlgorithmVersion:    SemanticIdentityAlgorithmV1,
		SemanticFingerprint: fingerprint,
		SemanticFindingID:   digestID("sf_", fullDigest("semantic-finding-id-v1", fingerprint)),
	}
}

func semanticFingerprintForRepositoryIdentity(repositoryIdentity, ruleID, anchor, instance string) string {
	targetDigest := fullDigest("semantic-target-v1", strings.TrimSpace(repositoryIdentity))
	return fullDigest(
		"semantic-fingerprint-v1",
		targetDigest,
		ruleID,
		anchor,
		instance,
	)
}

// DeriveCanonicalSemanticIdentity binds canonical semantic-v1 coordinates to
// an immutable repository identity selected by the trusted control plane. The
// caller-supplied semantic fingerprint is intentionally not an input.
func DeriveCanonicalSemanticIdentity(repositoryIdentity, ruleID, anchor, instance string) (SemanticIdentity, bool) {
	repositoryIdentity = strings.TrimSpace(repositoryIdentity)
	ruleID = semanticSlug(ruleID)
	anchor = semanticSlug(anchor)
	instance = semanticSlug(instance)
	if repositoryIdentity == "" || ruleID == "" || anchor == "" || instance == "" {
		return SemanticIdentity{}, false
	}
	fingerprint := semanticFingerprintForRepositoryIdentity(repositoryIdentity, ruleID, anchor, instance)
	return SemanticIdentity{
		RuleID:              ruleID,
		Anchor:              anchor,
		Instance:            instance,
		Quality:             store.IdentityQualityCanonical,
		AlgorithmVersion:    SemanticIdentityAlgorithmV1,
		SemanticFingerprint: fingerprint,
		SemanticFindingID:   digestID("sf_", fullDigest("semantic-finding-id-v1", fingerprint)),
	}, true
}

// OccurrenceID binds one run to the complete controller occurrence-group key.
// Canonical groups use the semantic fingerprint; producer-proposed groups add
// the exact normalized payload digest before calling this helper.
func OccurrenceID(runUID, observationGroupKey string) string {
	return digestID("occ_", fullDigest("finding-occurrence-v1", runUID, observationGroupKey))
}

func ObservationID(runUID, executionIdentity string, stagingGeneration int64, payloadDigest string, ordinal int) string {
	return digestID("obs_", fullDigest(
		"finding-observation-v1", runUID, executionIdentity,
		strings.TrimSpace(payloadDigest),
		strconv.FormatInt(stagingGeneration, 10),
		strconv.Itoa(ordinal),
	))
}

func SemanticFindingID(semanticFingerprint string) string {
	return digestID("sf_", fullDigest("semantic-finding-id-v1", semanticFingerprint))
}

// ProvisionalFindingFingerprint scopes a noncanonical materialized finding to
// one run and one exact observation group. Producer-proposed identities must
// not reuse a public finding projection across runs or distinct payloads.
func ProvisionalFindingFingerprint(runUID, observationGroupKey string) string {
	return fullDigest("provisional-finding-fingerprint-v1", runUID, observationGroupKey)
}

// ProvisionalFindingID returns a full-width public projection ID for a
// noncanonical observation group.
func ProvisionalFindingID(runUID, observationGroupKey string) string {
	return digestID("fnd_", fullDigest("provisional-finding-id-v1", runUID, observationGroupKey))
}
