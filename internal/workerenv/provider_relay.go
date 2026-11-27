/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workerenv

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

const AIProviderRelay = "ORKA_AI_PROVIDER_RELAY"

const ProviderRelayPrimary = "primary"

const providerRelayFallbackPrefix = "fallback-"

func ProviderRelayFallback(i int) string {
	return providerRelayFallbackPrefix + strconv.Itoa(i)
}

type ProviderRelayTarget struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

func EncodeProviderRelay(slots map[string]ProviderRelayTarget) (string, error) {
	if len(slots) == 0 {
		return "", nil
	}
	for slot, target := range slots {
		if err := validateProviderRelaySlot(slot, target); err != nil {
			return "", err
		}
	}
	encoded, err := json.Marshal(slots)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func ParseProviderRelay(value string) (map[string]ProviderRelayTarget, error) {
	if strings.TrimSpace(value) == "" {
		return map[string]ProviderRelayTarget{}, nil
	}
	var slots map[string]ProviderRelayTarget
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&slots); err != nil {
		return nil, errors.New(AIProviderRelay + " is not a valid provider relay map")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New(AIProviderRelay + " is not a valid provider relay map")
	}
	for slot, target := range slots {
		if err := validateProviderRelaySlot(slot, target); err != nil {
			return nil, err
		}
	}
	return slots, nil
}

func validateProviderRelaySlot(slot string, target ProviderRelayTarget) error {
	if !IsProviderRelaySlot(slot) {
		return fmt.Errorf("%s names an unknown slot %q", AIProviderRelay, slot)
	}
	if strings.TrimSpace(target.Namespace) == "" || strings.TrimSpace(target.Name) == "" || strings.TrimSpace(target.UID) == "" {
		return fmt.Errorf("%s slot %q needs a Provider namespace, name and UID", AIProviderRelay, slot)
	}
	return nil
}

func IsProviderRelaySlot(slot string) bool {
	if slot == ProviderRelayPrimary {
		return true
	}
	index, ok := strings.CutPrefix(slot, providerRelayFallbackPrefix)
	if !ok || index == "" || (len(index) > 1 && index[0] == '0') {
		return false
	}
	i, err := strconv.Atoi(index)
	return err == nil && i >= 0
}

func ProviderRelayURL(controllerURL, namespace, taskName, slot string) string {
	return strings.TrimRight(controllerURL, "/") + "/internal/v1/tasks/" + url.PathEscape(namespace) + "/" +
		url.PathEscape(taskName) + "/providers/" + url.PathEscape(slot)
}
