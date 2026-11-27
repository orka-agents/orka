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

func ValidateToolArguments(parameters *apiextensionsv1.JSON, arguments json.RawMessage) error {
	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = json.RawMessage(`{}`)
	}
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
