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
	"math/big"
	"strconv"

	"github.com/google/jsonschema-go/jsonschema"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// ValidateToolArguments checks a tool call's arguments against the Tool's
// declared JSON Schema before a person's credential is attached to the
// request. The worker that produced the arguments is not trusted to have
// honored the schema; the controller is the boundary.
func ValidateToolArguments(parameters *apiextensionsv1.JSON, arguments json.RawMessage) error {
	// An empty payload is the empty object, as the executor treats it, so a
	// parameterless call is judged rather than refused as malformed.
	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	// Numbers decode as float64 on purpose: the schema library judges
	// json.Number as a string, which would let "integer" constraints pass
	// for any value. The raw arguments, not this decoded copy, are executed,
	// so every number must survive that conversion exactly: a value the
	// schema would judge after rounding is refused outright.
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
	if err := numbersExactlyRepresentable(arguments); err != nil {
		return err
	}
	if parameters == nil || len(parameters.Raw) == 0 {
		return nil
	}
	// The schema's own numbers (const, enum, bounds) decode through float64
	// as well; a constraint that cannot be held exactly would be judged
	// after rounding, so such a schema is refused outright.
	if err := numbersExactlyRepresentable(parameters.Raw); err != nil {
		return fmt.Errorf("tool parameter schema: %w", err)
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

// numbersExactlyRepresentable walks a raw JSON document with exact numbers
// and refuses any that a float64 cannot hold exactly, since both the schema
// and the arguments are judged on float64 copies while the raw arguments are
// what the provider receives.
func numbersExactlyRepresentable(document json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var exact any
	if err := decoder.Decode(&exact); err != nil {
		return fmt.Errorf("decode document: %w", err)
	}
	var walk func(value any) error
	walk = func(value any) error {
		switch v := value.(type) {
		case json.Number:
			exact, ok := new(big.Rat).SetString(v.String())
			if !ok {
				return fmt.Errorf("number %q is not a valid number", v.String())
			}
			// A literal is judged faithfully when the float64 it decodes to
			// prints back (shortest round-trip form) as the same decimal
			// value: 0.1 does, 9007199254740993 (which rounds to ...992)
			// does not.
			approx, err := v.Float64()
			if err != nil {
				return fmt.Errorf("number %q is not exactly representable and cannot be validated", v.String())
			}
			roundTrip, ok := new(big.Rat).SetString(strconv.FormatFloat(approx, 'g', -1, 64))
			if !ok || roundTrip.Cmp(exact) != 0 {
				return fmt.Errorf("number %q is not exactly representable and cannot be validated", v.String())
			}
		case map[string]any:
			for _, item := range v {
				if err := walk(item); err != nil {
					return err
				}
			}
		case []any:
			for _, item := range v {
				if err := walk(item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(exact)
}
