/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/jsonschema-go/jsonschema"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// ValidateToolArguments checks a tool call's arguments against the Tool's
// declared JSON Schema before a person's credential is attached to the
// request. The worker that produced the arguments is not trusted to have
// honored the schema; the controller is the boundary.
func ValidateToolArguments(parameters *apiextensionsv1.JSON, arguments json.RawMessage) error {
	// Numbers decode as float64 on purpose: the schema library judges
	// json.Number as a string, which would let "integer" constraints pass
	// for any value. The raw arguments, not this decoded copy, are executed.
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	var instance any
	if err := decoder.Decode(&instance); err != nil {
		return fmt.Errorf("decode arguments: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("arguments contain more than one JSON value")
	}
	if _, ok := instance.(map[string]any); !ok {
		return errors.New("arguments must be a JSON object")
	}
	if parameters == nil || len(parameters.Raw) == 0 {
		return nil
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(parameters.Raw, &schema); err != nil {
		return fmt.Errorf("decode tool parameter schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return fmt.Errorf("resolve tool parameter schema: %w", err)
	}
	if err := resolved.Validate(instance); err != nil {
		return fmt.Errorf("arguments do not satisfy the tool's parameter schema: %w", err)
	}
	return nil
}
