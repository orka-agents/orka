/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const digestPrefix = "sha256:"

// DeriveTenantID deterministically binds a tenant to one cluster and namespace
// incarnation without exposing an ambiguous delimiter encoding.
func DeriveTenantID(clusterID, namespaceUID string) string {
	sum := sha256.Sum256([]byte(clusterID + "\x00" + namespaceUID))
	return "orka-tenant-sha256:" + hex.EncodeToString(sum[:])
}

// CanonicalUpsertKey returns the only accepted provider upsert key.
func CanonicalUpsertKey(binding Binding, memoryID string) string {
	return "orka:" + binding.ClusterID + ":" + binding.NamespaceUID + ":" +
		strconv.FormatUint(binding.AuthorityEpoch, 10) + ":" + memoryID
}

// ContentDigest is SHA-256 over the exact UTF-8 content bytes. It performs no
// Unicode normalization or line-ending rewriting.
func ContentDigest(content string) string {
	return digestBytes([]byte(content))
}

// MemoryRecordDigest hashes the complete canonical wire representation of one
// validated memory record. The binding is used to prove the record's upsert key
// belongs to the expected authority before any digest is produced.
func MemoryRecordDigest(record *MemoryRecord, binding Binding) (string, error) {
	if err := ValidateMemoryRecord(record, binding); err != nil {
		return "", err
	}
	data, err := canonicalJSON(record)
	if err != nil {
		return "", fmt.Errorf("canonical memory record: %w", err)
	}
	return digestBytes(data), nil
}

// EmptyContentDigest is the required content digest for a tombstone mutation.
func EmptyContentDigest() string {
	return ContentDigest("")
}

// BindingDigest covers the complete authority and routing identity echoed by a
// response.
func BindingDigest(binding Binding) string {
	data, _ := canonicalJSON(binding)
	return digestBytes(data)
}

// AuthorityDigest excludes only RoutingEpoch and identifies durable records
// within one claimed authority.
func AuthorityDigest(binding Binding) string {
	value := struct {
		ClusterID      string `json:"clusterId"`
		NamespaceUID   string `json:"namespaceUid"`
		BackendUID     string `json:"backendUid"`
		AuthorityEpoch uint64 `json:"authorityEpoch"`
		TenantID       string `json:"tenantId"`
		StoreUUID      string `json:"storeUuid"`
	}{
		ClusterID: binding.ClusterID, NamespaceUID: binding.NamespaceUID,
		BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch,
		TenantID: binding.TenantID, StoreUUID: binding.StoreUUID,
	}
	data, _ := canonicalJSON(value)
	return digestBytes(data)
}

// ClaimScopeDigest identifies the exclusive writer slot. BackendUID is omitted
// deliberately so a second backend cannot establish a parallel owner for the
// same tenant, store, and authority epoch.
func ClaimScopeDigest(binding Binding) string {
	value := struct {
		TenantID       string `json:"tenantId"`
		StoreUUID      string `json:"storeUuid"`
		AuthorityEpoch uint64 `json:"authorityEpoch"`
	}{TenantID: binding.TenantID, StoreUUID: binding.StoreUUID, AuthorityEpoch: binding.AuthorityEpoch}
	data, _ := canonicalJSON(value)
	return digestBytes(data)
}

// NormalizeTags returns the canonical lower-case, trimmed, deduplicated, sorted
// representation used on the wire and in mutation digests.
func NormalizeTags(tags []string) ([]string, error) {
	if tags == nil {
		return nil, errors.New("tags must be an explicit array")
	}
	if len(tags) > MaxTags {
		return nil, fmt.Errorf("tags exceeds %d entries", MaxTags)
	}
	set := make(map[string]struct{}, len(tags))
	for _, raw := range tags {
		tag := strings.ToLower(strings.TrimSpace(raw))
		if err := validateTag(tag); err != nil {
			return nil, err
		}
		set[tag] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for tag := range set {
		result = append(result, tag)
	}
	sort.Strings(result)
	return result, nil
}

// NormalizeMetadata returns canonical lower-case keys and trimmed values. A
// normalization collision is rejected instead of silently overwriting data.
func NormalizeMetadata(metadata map[string]string) (map[string]string, error) {
	if metadata == nil {
		return nil, errors.New("metadata must be an explicit object")
	}
	if len(metadata) > MaxMetadataEntries {
		return nil, fmt.Errorf("metadata exceeds %d entries", MaxMetadataEntries)
	}
	result := make(map[string]string, len(metadata))
	for rawKey, rawValue := range metadata {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		value := strings.TrimSpace(rawValue)
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("metadata contains duplicate normalized key %q", truncateForError(key))
		}
		if err := validateMetadataEntry(key, value); err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, nil
}

// PrepareMutation canonicalizes caller-owned collections and fills all derived
// identity and digest fields. Content bytes are never changed.
func PrepareMutation(envelope *MutationEnvelope) error {
	if envelope == nil {
		return errors.New("mutation envelope is required")
	}
	envelope.ProtocolVersion = strings.TrimSpace(envelope.ProtocolVersion)
	envelope.OperationID = strings.TrimSpace(envelope.OperationID)
	envelope.MemoryID = strings.TrimSpace(envelope.MemoryID)
	envelope.Kind = strings.TrimSpace(envelope.Kind)
	envelope.ExpectedBackendVersion = strings.TrimSpace(envelope.ExpectedBackendVersion)
	if envelope.Binding.TenantID == "" {
		envelope.Binding.TenantID = DeriveTenantID(envelope.Binding.ClusterID, envelope.Binding.NamespaceUID)
	}
	if envelope.UpsertKey == "" {
		envelope.UpsertKey = CanonicalUpsertKey(envelope.Binding, envelope.MemoryID)
	}
	if envelope.Kind == MutationKindDelete {
		if envelope.State != nil {
			return errors.New("delete state must be null")
		}
		envelope.ContentDigest = EmptyContentDigest()
	} else {
		if envelope.State == nil {
			return errors.New("create/replace state is required")
		}
		tags, err := NormalizeTags(envelope.State.Tags)
		if err != nil {
			return err
		}
		metadata, err := NormalizeMetadata(envelope.State.Metadata)
		if err != nil {
			return err
		}
		envelope.State.Tags = tags
		envelope.State.Metadata = metadata
		envelope.ContentDigest = ContentDigest(envelope.State.Content)
	}
	digest, err := MutationDigest(envelope)
	if err != nil {
		return err
	}
	envelope.MutationDigest = digest
	return ValidateMutationEnvelope(envelope)
}

// MutationDigest hashes the complete versioned canonical mutation preimage. The
// mutationDigest field itself is intentionally excluded.
func MutationDigest(envelope *MutationEnvelope) (string, error) {
	if envelope == nil {
		return "", errors.New("mutation envelope is required")
	}
	preimage := struct {
		ProtocolVersion        string         `json:"protocolVersion"`
		OperationID            string         `json:"operationId"`
		Binding                Binding        `json:"binding"`
		MemoryID               string         `json:"memoryId"`
		UpsertKey              string         `json:"upsertKey"`
		Kind                   string         `json:"kind"`
		Generation             uint64         `json:"generation"`
		ExpectedGeneration     uint64         `json:"expectedGeneration"`
		ExpectedBackendVersion string         `json:"expectedBackendVersion"`
		ContentDigest          string         `json:"contentDigest"`
		State                  *MutationState `json:"state"`
	}{
		ProtocolVersion: envelope.ProtocolVersion, OperationID: envelope.OperationID,
		Binding: envelope.Binding, MemoryID: envelope.MemoryID, UpsertKey: envelope.UpsertKey,
		Kind: envelope.Kind, Generation: envelope.Generation,
		ExpectedGeneration:     envelope.ExpectedGeneration,
		ExpectedBackendVersion: envelope.ExpectedBackendVersion,
		ContentDigest:          envelope.ContentDigest, State: envelope.State,
	}
	data, err := canonicalJSON(preimage)
	if err != nil {
		return "", fmt.Errorf("canonical mutation: %w", err)
	}
	return digestBytes(data), nil
}

func canonicalJSON(value any) ([]byte, error) {
	return EncodeJSON(value)
}

// EncodeJSON returns one compact JSON value with HTML escaping disabled. OMS
// digests, persisted receipts, and wire messages use the same representation
// instead of encoding/json's larger HTML-safe form. Callers still enforce the
// operation-specific MaxHTTPBodyBytes or MaxAdapterResponseBytes hard bound.
func EncodeJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return digestPrefix + hex.EncodeToString(sum[:])
}
