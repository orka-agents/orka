/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

type jsonShape struct {
	required []string
	optional []string
	children map[string]jsonShape
	arrays   map[string]jsonShape
	nullable bool
}

var bindingShape = jsonShape{required: []string{
	"clusterId", "namespaceUid", "backendUid", "authorityEpoch", "routingEpoch", "tenantId", "storeUuid",
}}

var storeResolutionBindingShape = jsonShape{required: []string{
	"clusterId", "namespaceUid", "backendUid", "tenantId",
}}

var capabilitiesShape = jsonShape{required: []string{
	"durableIdempotency", "idempotencyDigestConflicts", "createIfAbsent", "conditionalMutation",
	"monotonicGenerations", "deleteHighWatermark", "durableRoutingFence", "operationLookup", "exactGet",
	"stablePagination", "exclusiveOwnership", "keywordSearch", "auditVersionVisibility", "semanticSearch", "hybridSearch",
}}

var limitsShape = jsonShape{required: []string{
	"maxRequestBytes", "maxResponseBytes", "maxContentBytes", "maxTags", "maxTagBytes", "maxMetadataEntries",
	"maxMetadataKeyBytes", "maxMetadataValueBytes", "maxQueryBytes", "maxPageSize", "maxSnapshotRecords",
	"snapshotTtlSeconds",
}}

var stateShape = jsonShape{required: []string{"content", "tags", "metadata"}, nullable: true}

var receiptShape = jsonShape{
	required: []string{"protocolVersion", "binding", "result", "operationId", "bindingDigest", "appliedGeneration",
		"backendVersion", "backendMemoryId", "contentDigest", "mutationDigest", "completedAt"},
	children: map[string]jsonShape{"binding": bindingShape},
}

var recordShape = jsonShape{
	required: []string{"memoryId", "upsertKey", "state", "generation", "backendVersion", "backendMemoryId",
		"contentDigest", "content", "tags", "metadata", "updatedAt"},
	optional: []string{"score"},
}

func DecodeHealthResponse(body []byte) (*HealthResponse, error) {
	var response HealthResponse
	if err := decodeClosed(
		body,
		MaxAdapterResponseBytes,
		&response,
		jsonShape{required: []string{"protocolVersion", "status"}},
	); err != nil {
		return nil, fmt.Errorf("invalid health response: %w", err)
	}
	if response.ProtocolVersion != Version || response.Status != "ok" {
		return nil, errors.New("invalid health response values")
	}
	return &response, nil
}

func DecodeStoreResolveRequest(body []byte) (*StoreResolveRequest, error) {
	var request StoreResolveRequest
	shape := jsonShape{
		required: []string{"protocolVersion", "binding", "storeName"},
		children: map[string]jsonShape{"binding": storeResolutionBindingShape},
	}
	if err := decodeClosed(body, MaxHTTPBodyBytes, &request, shape); err != nil {
		return nil, fmt.Errorf("invalid store resolve request: %w", err)
	}
	if err := ValidateStoreResolveRequest(&request); err != nil {
		return nil, err
	}
	return &request, nil
}

func DecodeStoreResolveResponse(body []byte) (*StoreResolveResponse, error) {
	var response StoreResolveResponse
	shape := jsonShape{
		required: []string{"protocolVersion", "binding", "storeName", "storeUuid"},
		children: map[string]jsonShape{"binding": storeResolutionBindingShape},
	}
	if err := decodeClosed(body, MaxAdapterResponseBytes, &response, shape); err != nil {
		return nil, fmt.Errorf("invalid store resolve response: %w", err)
	}
	if err := ValidateStoreResolveResponse(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func DecodeCapabilitiesRequest(body []byte) (*CapabilitiesRequest, error) {
	var request CapabilitiesRequest
	shape := jsonShape{
		required: []string{"protocolVersion", "binding"},
		children: map[string]jsonShape{"binding": bindingShape},
	}
	if err := decodeClosed(body, MaxHTTPBodyBytes, &request, shape); err != nil {
		return nil, fmt.Errorf("invalid capabilities request: %w", err)
	}
	if err := ValidateCapabilitiesRequest(&request); err != nil {
		return nil, err
	}
	return &request, nil
}

func DecodeCapabilitiesResponse(body []byte) (*CapabilitiesResponse, error) {
	var response CapabilitiesResponse
	shape := jsonShape{
		required: []string{
			"protocolVersion", "binding", "adapterName", "adapterVersion", "revision", "expiresAt", "capabilities",
			"limits",
		},
		children: map[string]jsonShape{"binding": bindingShape, "capabilities": capabilitiesShape, "limits": limitsShape},
	}
	if err := decodeClosed(body, MaxAdapterResponseBytes, &response, shape); err != nil {
		return nil, fmt.Errorf("invalid capabilities response: %w", err)
	}
	if err := ValidateCapabilitiesResponse(&response, time.Now()); err != nil {
		return nil, err
	}
	return &response, nil
}

func DecodeOwnershipClaimRequest(body []byte) (*OwnershipClaimRequest, error) {
	var request OwnershipClaimRequest
	shape := jsonShape{
		required: []string{"protocolVersion", "binding"},
		children: map[string]jsonShape{"binding": bindingShape},
	}
	if err := decodeClosed(body, MaxHTTPBodyBytes, &request, shape); err != nil {
		return nil, fmt.Errorf("invalid ownership claim request: %w", err)
	}
	if err := ValidateOwnershipClaimRequest(&request); err != nil {
		return nil, err
	}
	return &request, nil
}

func DecodeOwnershipClaimResponse(body []byte) (*OwnershipClaimResponse, error) {
	var response OwnershipClaimResponse
	shape := jsonShape{
		required: []string{
			"protocolVersion", "binding", "result", "bindingDigest", "claimIdentity", "maximumRoutingEpoch", "claimedAt",
		},
		children: map[string]jsonShape{"binding": bindingShape},
	}
	if err := decodeClosed(body, MaxAdapterResponseBytes, &response, shape); err != nil {
		return nil, fmt.Errorf("invalid ownership claim response: %w", err)
	}
	if err := ValidateOwnershipClaimResponse(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func DecodeRoutingFenceRequest(body []byte) (*RoutingFenceRequest, error) {
	var request RoutingFenceRequest
	shape := jsonShape{
		required: []string{"protocolVersion", "binding"},
		children: map[string]jsonShape{"binding": bindingShape},
	}
	if err := decodeClosed(body, MaxHTTPBodyBytes, &request, shape); err != nil {
		return nil, fmt.Errorf("invalid routing fence request: %w", err)
	}
	if err := ValidateRoutingFenceRequest(&request); err != nil {
		return nil, err
	}
	return &request, nil
}

func DecodeRoutingFenceResponse(body []byte) (*RoutingFenceResponse, error) {
	var response RoutingFenceResponse
	shape := jsonShape{
		required: []string{"protocolVersion", "binding", "result", "bindingDigest", "maximumRoutingEpoch", "completedAt"},
		children: map[string]jsonShape{"binding": bindingShape},
	}
	if err := decodeClosed(body, MaxAdapterResponseBytes, &response, shape); err != nil {
		return nil, fmt.Errorf("invalid routing fence response: %w", err)
	}
	if err := ValidateRoutingFenceResponse(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func DecodeMutationEnvelope(body []byte) (*MutationEnvelope, error) {
	var envelope MutationEnvelope
	shape := jsonShape{
		required: []string{"protocolVersion", "operationId", "binding", "memoryId", "upsertKey", "kind", "generation",
			"expectedGeneration", "expectedBackendVersion", "contentDigest", "mutationDigest", "state"},
		children: map[string]jsonShape{"binding": bindingShape, "state": stateShape},
	}
	if err := decodeClosed(body, MaxHTTPBodyBytes, &envelope, shape); err != nil {
		return nil, fmt.Errorf("invalid mutation envelope: %w", err)
	}
	if err := ValidateMutationEnvelope(&envelope); err != nil {
		return nil, err
	}
	return &envelope, nil
}

func DecodeMutationReceipt(body []byte) (*MutationReceipt, error) {
	var receipt MutationReceipt
	if err := decodeClosed(body, MaxAdapterResponseBytes, &receipt, receiptShape); err != nil {
		return nil, fmt.Errorf("invalid mutation receipt: %w", err)
	}
	if err := ValidateMutationReceipt(&receipt); err != nil {
		return nil, err
	}
	return &receipt, nil
}

func DecodeGetRequest(body []byte) (*GetRequest, error) {
	var request GetRequest
	shape := jsonShape{
		required: []string{"protocolVersion", "binding", "upsertKey"},
		children: map[string]jsonShape{"binding": bindingShape},
	}
	if err := decodeClosed(body, MaxHTTPBodyBytes, &request, shape); err != nil {
		return nil, fmt.Errorf("invalid get request: %w", err)
	}
	if err := ValidateGetRequest(&request); err != nil {
		return nil, err
	}
	return &request, nil
}

func DecodeGetResponse(body []byte) (*GetResponse, error) {
	var response GetResponse
	shape := jsonShape{
		required: []string{"protocolVersion", "binding", "found", "record"},
		children: map[string]jsonShape{"binding": bindingShape, "record": withNullable(recordShape)},
	}
	if err := decodeClosed(body, MaxAdapterResponseBytes, &response, shape); err != nil {
		return nil, fmt.Errorf("invalid get response: %w", err)
	}
	if err := ValidateGetResponse(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func DecodeOperationLookupRequest(body []byte) (*OperationLookupRequest, error) {
	var request OperationLookupRequest
	shape := jsonShape{
		required: []string{"protocolVersion", "binding", "operationId"},
		children: map[string]jsonShape{"binding": bindingShape},
	}
	if err := decodeClosed(body, MaxHTTPBodyBytes, &request, shape); err != nil {
		return nil, fmt.Errorf("invalid operation lookup request: %w", err)
	}
	if err := ValidateOperationLookupRequest(&request); err != nil {
		return nil, err
	}
	return &request, nil
}

func DecodeOperationLookupResponse(body []byte) (*OperationLookupResponse, error) {
	var response OperationLookupResponse
	shape := jsonShape{
		required: []string{"protocolVersion", "binding", "found", "receipt"},
		children: map[string]jsonShape{"binding": bindingShape, "receipt": withNullable(receiptShape)},
	}
	if err := decodeClosed(body, MaxAdapterResponseBytes, &response, shape); err != nil {
		return nil, fmt.Errorf("invalid operation lookup response: %w", err)
	}
	if err := ValidateOperationLookupResponse(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func DecodeSearchRequest(body []byte) (*SearchRequest, error) {
	var request SearchRequest
	shape := jsonShape{
		required: []string{"protocolVersion", "binding", "mode", "query", "pageSize", "pageToken"},
		children: map[string]jsonShape{"binding": bindingShape},
	}
	if err := decodeClosed(body, MaxHTTPBodyBytes, &request, shape); err != nil {
		return nil, fmt.Errorf("invalid search request: %w", err)
	}
	if err := ValidateSearchRequest(&request); err != nil {
		return nil, err
	}
	return &request, nil
}

func DecodeSearchResponse(body []byte) (*SearchResponse, error) {
	var response SearchResponse
	shape := jsonShape{
		required: []string{
			"protocolVersion", "binding", "requestedMode", "actualMode", "records", "nextPageToken", "exhausted",
			"snapshotExpiresAt",
		},
		children: map[string]jsonShape{"binding": bindingShape},
		arrays:   map[string]jsonShape{"records": recordShape},
	}
	if err := decodeClosed(body, MaxAdapterResponseBytes, &response, shape); err != nil {
		return nil, fmt.Errorf("invalid search response: %w", err)
	}
	if err := ValidateSearchResponse(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func DecodeErrorResponse(body []byte) (*ErrorResponse, error) {
	var response ErrorResponse
	shape := jsonShape{
		required: []string{"protocolVersion", "binding", "code", "message", "retryable", "retryAfterSeconds"},
		children: map[string]jsonShape{"binding": withNullable(bindingShape)},
	}
	if err := decodeClosed(body, MaxAdapterResponseBytes, &response, shape); err != nil {
		return nil, fmt.Errorf("invalid error response: %w", err)
	}
	if err := ValidateErrorResponse(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func withNullable(shape jsonShape) jsonShape {
	shape.nullable = true
	return shape
}

func decodeClosed(body []byte, limit int, target any, shape jsonShape) error {
	if len(body) == 0 {
		return errors.New("body is required")
	}
	if len(body) > limit {
		return fmt.Errorf("body exceeds %d bytes", limit)
	}
	if !utf8.Valid(body) {
		return errors.New("body must be valid UTF-8")
	}
	if err := validateJSONUnicodeEscapes(body); err != nil {
		return err
	}
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return err
	}
	if err := validateJSONShape(body, shape); err != nil {
		return err
	}
	if err := ValidateJSONNullability(body, target); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

// ValidateJSONNullability rejects JSON null for every non-pointer Go field in
// target, including nested struct fields, typed map values, and array/slice
// items. Pointer fields remain explicitly nullable and semantic validators
// decide when their null value is allowed by the operation.
func ValidateJSONNullability(body []byte, target any) error {
	typeOfTarget := reflect.TypeOf(target)
	if typeOfTarget == nil || typeOfTarget.Kind() != reflect.Pointer {
		return errors.New("JSON nullability target must be a non-nil pointer")
	}
	return validateJSONNullability(body, typeOfTarget.Elem(), "value")
}

var jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()

func validateJSONNullability(raw []byte, targetType reflect.Type, path string) error {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		if targetType.Kind() == reflect.Pointer || targetType.Kind() == reflect.Interface {
			return nil
		}
		return fmt.Errorf("%s must not be null", path)
	}
	for targetType.Kind() == reflect.Pointer {
		targetType = targetType.Elem()
	}
	if reflect.PointerTo(targetType).Implements(jsonUnmarshalerType) {
		return nil
	}
	switch targetType.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return err
		}
		if object == nil {
			return fmt.Errorf("%s must be an object", path)
		}
		for field := range targetType.Fields() {
			if field.PkgPath != "" {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			value, found := object[name]
			if !found {
				continue
			}
			if err := validateJSONNullability(value, field.Type, path+"."+name); err != nil {
				return err
			}
		}
	case reflect.Map:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return err
		}
		if object == nil {
			return fmt.Errorf("%s must not be null", path)
		}
		for key, value := range object {
			if err := validateJSONNullability(value, targetType.Elem(), fmt.Sprintf("%s[%q]", path, key)); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var elements []json.RawMessage
		if err := json.Unmarshal(trimmed, &elements); err != nil {
			return err
		}
		if elements == nil {
			return fmt.Errorf("%s must not be null", path)
		}
		for index, value := range elements {
			if err := validateJSONNullability(value, targetType.Elem(), fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateJSONShape(raw []byte, shape jsonShape) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if shape.nullable {
			return nil
		}
		return errors.New("object must not be null")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	if object == nil {
		return errors.New("JSON value must be an object")
	}
	allowed := make(map[string]struct{}, len(shape.required)+len(shape.optional)+len(shape.children)+len(shape.arrays))
	for _, field := range shape.required {
		allowed[field] = struct{}{}
		if _, ok := object[field]; !ok {
			return fmt.Errorf("required field %q is missing", field)
		}
	}
	for _, field := range shape.optional {
		allowed[field] = struct{}{}
	}
	for field := range shape.children {
		allowed[field] = struct{}{}
	}
	for field := range shape.arrays {
		allowed[field] = struct{}{}
	}
	for field := range object {
		if _, ok := allowed[field]; !ok {
			return fmt.Errorf("unknown field %q", field)
		}
	}
	for field, child := range shape.children {
		value, ok := object[field]
		if !ok {
			continue
		}
		if err := validateJSONShape(value, child); err != nil {
			return fmt.Errorf("field %q: %w", field, err)
		}
	}
	for field, elementShape := range shape.arrays {
		value, ok := object[field]
		if !ok {
			continue
		}
		var elements []json.RawMessage
		if err := json.Unmarshal(value, &elements); err != nil {
			return fmt.Errorf("field %q: must be an array: %w", field, err)
		}
		if elements == nil {
			return fmt.Errorf("field %q: array must not be null", field)
		}
		for index := range elements {
			if err := validateJSONShape(elements[index], elementShape); err != nil {
				return fmt.Errorf("field %q item %d: %w", field, index, err)
			}
		}
	}
	return nil
}

func validateJSONUnicodeEscapes(body []byte) error {
	for index := 0; index < len(body); {
		if body[index] != '"' {
			index++
			continue
		}
		index++
		for {
			if index >= len(body) {
				return errors.New("unterminated JSON string")
			}
			switch body[index] {
			case '"':
				index++
				goto nextString
			case '\\':
				if index+1 >= len(body) {
					return errors.New("unterminated JSON escape")
				}
				if body[index+1] != 'u' {
					index += 2
					continue
				}
				value, next, ok := decodeHexEscape(body, index+2)
				if !ok {
					return errors.New("invalid JSON Unicode escape")
				}
				index = next
				if value >= 0xD800 && value <= 0xDBFF {
					if index+6 > len(body) || body[index] != '\\' || body[index+1] != 'u' {
						return errors.New("unpaired high surrogate in JSON string")
					}
					low, lowNext, ok := decodeHexEscape(body, index+2)
					if !ok || low < 0xDC00 || low > 0xDFFF {
						return errors.New("unpaired high surrogate in JSON string")
					}
					index = lowNext
				} else if value >= 0xDC00 && value <= 0xDFFF {
					return errors.New("unpaired low surrogate in JSON string")
				}
			default:
				if body[index] < 0x20 {
					return errors.New("unescaped control byte in JSON string")
				}
				index++
			}
		}
	nextString:
	}
	return nil
}

func decodeHexEscape(body []byte, start int) (uint16, int, bool) {
	if start+4 > len(body) {
		return 0, start, false
	}
	var value uint16
	for index := start; index < start+4; index++ {
		value <<= 4
		switch char := body[index]; {
		case char >= '0' && char <= '9':
			value |= uint16(char - '0')
		case char >= 'a' && char <= 'f':
			value |= uint16(char-'a') + 10
		case char >= 'A' && char <= 'F':
			value |= uint16(char-'A') + 10
		default:
			return 0, start, false
		}
	}
	return value, start + 4, true
}

func rejectDuplicateJSONKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if token, err := decoder.Token(); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("invalid trailing JSON: %w", err)
	} else {
		return fmt.Errorf("unexpected trailing JSON token %v", token)
	}
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key must be a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("invalid object terminator")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("invalid array terminator")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return errors.New("body must contain exactly one JSON value")
}
