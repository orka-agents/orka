package artifactcap

import (
	"errors"
	"fmt"
	"mime"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	httpMethodGet                          = "GET"
	httpMethodPut                          = "PUT"
	CapabilityVersion                      = "orka.artifact.capability.v1"
	CapabilityAudience                     = "orka.acp.artifact-api"
	MinSecretBytes                         = 32
	MaxCapabilityTTL                       = 5 * time.Minute
	MaxClockSkew                           = 30 * time.Second
	CapabilityHeader                       = "X-Orka-Artifact-Capability"
	RequestDigestHeader                    = "X-Orka-Artifact-Request-Digest"
	ContentLengthHeader                    = "X-Orka-Artifact-Content-Length"
	MediaTypeHeader                        = "X-Orka-Artifact-Media-Type"
	ObjectDigestHeader                     = "X-Orka-Artifact-Digest"
	MediaTypeWorkspaceTar                  = "application/vnd.orka.workspace.v1+tar"
	MediaTypeWorkspaceDelta                = "application/vnd.orka.workspace-delta.v1+tar"
	MediaTypeGitBundle                     = "application/vnd.orka.git-bundle.v1"
	DefaultWorkspaceArtifactMaxBytes int64 = 100 << 20
)

var (
	ErrUnauthorized      = errors.New("artifact capability authorization failed")
	ErrExpired           = errors.New("artifact capability expired")
	ErrInvalidRequest    = errors.New("invalid artifact request")
	ErrReplay            = errors.New("artifact operation already used")
	ErrOperationConflict = errors.New("artifact operation conflicts with durable replay record")
	ErrTooLarge          = errors.New("artifact exceeds configured size limit")
	ErrPartialUpload     = errors.New("artifact upload ended before the declared content length")
	ErrDigestMismatch    = errors.New("artifact digest mismatch")
	ErrNotFound          = errors.New("artifact not found")
	ErrUnsafePath        = errors.New("unsafe artifact storage path")
	ErrCorrupt           = errors.New("artifact storage is corrupt")
)

var safeIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)

type Operation string

const (
	OperationUpload   Operation = "upload"
	OperationDownload Operation = "download"
)

func (o Operation) Validate() error {
	switch o {
	case OperationUpload, OperationDownload:
		return nil
	default:
		return fmt.Errorf("%w: unsupported operation %q", ErrInvalidRequest, o)
	}
}

func (o Operation) Method() string {
	if o == OperationUpload {
		return httpMethodPut
	}
	if o == OperationDownload {
		return httpMethodGet
	}
	return ""
}

type Identity struct {
	Namespace     string `json:"namespace"`
	TaskID        string `json:"taskID,omitempty"`
	PublicationID string `json:"publicationID,omitempty"`
}

func (i Identity) Validate() error {
	if err := validateIdentifier("namespace", i.Namespace, 253); err != nil {
		return err
	}
	if (i.TaskID == "") == (i.PublicationID == "") {
		return fmt.Errorf("%w: exactly one task or publication identity is required", ErrInvalidRequest)
	}
	if i.TaskID != "" {
		return validateIdentifier("task identity", i.TaskID, 512)
	}
	return validateIdentifier("publication identity", i.PublicationID, 512)
}

type OperationRequest struct {
	Operation     Operation `json:"operation"`
	ObjectDigest  string    `json:"objectDigest"`
	Identity      Identity  `json:"identity"`
	ContentLength int64     `json:"contentLength"`
	MediaType     string    `json:"mediaType"`
	OperationID   string    `json:"operationID"`
}

func (r OperationRequest) Validate() error {
	if err := r.Operation.Validate(); err != nil {
		return err
	}
	if err := ValidateObjectDigest(r.ObjectDigest); err != nil {
		return err
	}
	if err := r.Identity.Validate(); err != nil {
		return err
	}
	if r.ContentLength < 0 {
		return fmt.Errorf("%w: content length must be non-negative", ErrInvalidRequest)
	}
	if err := ValidateMediaType(r.MediaType); err != nil {
		return err
	}
	return validateIdentifier("operation ID", r.OperationID, 512)
}

func (r OperationRequest) Method() string { return r.Operation.Method() }

func (r OperationRequest) Path() string {
	value, _ := ObjectPath(r.ObjectDigest)
	return value
}

func ObjectPath(digest string) (string, error) {
	hexDigest, err := DigestHex(digest)
	if err != nil {
		return "", err
	}
	return path.Join("/internal/v2/acp/artifacts/sha256", hexDigest), nil
}

type Authorization struct {
	Capability    string
	RequestDigest string
}

type CapabilityClaims struct {
	Version       string           `json:"version"`
	Audience      string           `json:"audience"`
	Request       OperationRequest `json:"request"`
	RequestDigest string           `json:"requestDigest"`
	IssuedAt      time.Time        `json:"issuedAt"`
	ExpiresAt     time.Time        `json:"expiresAt"`
}

func (c CapabilityClaims) ValidateAt(now time.Time) error {
	if c.Version != CapabilityVersion || c.Audience != CapabilityAudience {
		return ErrUnauthorized
	}
	if err := c.Request.Validate(); err != nil {
		return ErrUnauthorized
	}
	if !IsRequestDigest(c.RequestDigest) {
		return ErrUnauthorized
	}
	if c.IssuedAt.IsZero() || c.ExpiresAt.IsZero() || !c.ExpiresAt.After(c.IssuedAt) || c.ExpiresAt.Sub(c.IssuedAt) > MaxCapabilityTTL {
		return ErrUnauthorized
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if c.IssuedAt.After(now.Add(MaxClockSkew)) {
		return ErrUnauthorized
	}
	if !c.ExpiresAt.After(now) {
		return ErrExpired
	}
	return nil
}

type PresentedRequest struct {
	Method        string
	Path          string
	ObjectDigest  string
	ContentLength int64
	MediaType     string
	RequestDigest string
}

func (r PresentedRequest) Validate() error {
	if r.Method != "PUT" && r.Method != "GET" {
		return fmt.Errorf("%w: unsupported HTTP method", ErrInvalidRequest)
	}
	if err := ValidateObjectDigest(r.ObjectDigest); err != nil {
		return err
	}
	if r.Path == "" || !strings.HasPrefix(r.Path, "/") || path.Clean(r.Path) != r.Path {
		return fmt.Errorf("%w: non-canonical request path", ErrInvalidRequest)
	}
	if r.ContentLength < 0 {
		return fmt.Errorf("%w: content length must be non-negative", ErrInvalidRequest)
	}
	if err := ValidateMediaType(r.MediaType); err != nil {
		return err
	}
	if !IsRequestDigest(r.RequestDigest) {
		return fmt.Errorf("%w: invalid request digest", ErrInvalidRequest)
	}
	return nil
}

type Artifact struct {
	ArtifactID string    `json:"artifactID"`
	Digest     string    `json:"digest"`
	SizeBytes  int64     `json:"sizeBytes"`
	MediaType  string    `json:"mediaType"`
	CreatedAt  time.Time `json:"createdAt"`
}

func (a Artifact) Validate() error {
	if err := validateIdentifier("artifact ID", a.ArtifactID, 128); err != nil {
		return err
	}
	if err := ValidateObjectDigest(a.Digest); err != nil {
		return err
	}
	if a.SizeBytes < 0 {
		return fmt.Errorf("%w: artifact size must be non-negative", ErrInvalidRequest)
	}
	return ValidateMediaType(a.MediaType)
}

func ArtifactIDForDigest(digest string) (string, error) {
	hexDigest, err := DigestHex(digest)
	if err != nil {
		return "", err
	}
	return "sha256-" + hexDigest, nil
}

func ValidateMediaType(value string) error {
	if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%w: invalid media type", ErrInvalidRequest)
	}
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil || mediaType == "" {
		return fmt.Errorf("%w: invalid media type", ErrInvalidRequest)
	}
	if canonical := mime.FormatMediaType(mediaType, params); canonical != value {
		return fmt.Errorf("%w: media type must be canonical", ErrInvalidRequest)
	}
	return nil
}

func validateIdentifier(name, value string, max int) error {
	if value == "" || len(value) > max || !safeIdentifierPattern.MatchString(value) || strings.Contains(value, "..") {
		return fmt.Errorf("%w: invalid %s", ErrInvalidRequest, name)
	}
	return nil
}
