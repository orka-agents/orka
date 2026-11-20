/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workerenv

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// AIProviderRelay lists the model provider slots whose requests the AI
// worker sends through the controller's relay instead of to the Provider
// with a key. The controller sets it on every AI worker Job from the
// Providers resolved at dispatch; the value is JSON mapping each relayed slot
// to the Provider it was built for. The Job spec is immutable, so the relay
// reads the same value back to decide which Provider a slot may reach.
const AIProviderRelay = "ORKA_AI_PROVIDER_RELAY"

// ProviderRelayPrimary is the slot of the Task's own Provider.
const ProviderRelayPrimary = "primary"

const providerRelayFallbackPrefix = "fallback-"

// ProviderRelayFallback is the slot of the Agent's i-th fallback Provider.
func ProviderRelayFallback(i int) string {
	return providerRelayFallbackPrefix + strconv.Itoa(i)
}

// ProviderRelayTarget identifies the Provider a relayed slot was built for.
// The relay refuses a slot whose Provider was deleted and recreated, because
// the UID then differs.
type ProviderRelayTarget struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// EncodeProviderRelay renders the AIProviderRelay value; no slots render as
// the empty string.
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

// ParseProviderRelay reads the AIProviderRelay value. An empty value means no
// slot is relayed; a value that does not parse is an error, so neither side
// guesses which Provider a slot is for.
func ParseProviderRelay(value string) (map[string]ProviderRelayTarget, error) {
	if strings.TrimSpace(value) == "" {
		return map[string]ProviderRelayTarget{}, nil
	}
	var slots map[string]ProviderRelayTarget
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&slots); err != nil || decoder.More() {
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

// IsProviderRelaySlot reports whether slot is "primary" or "fallback-<i>".
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

// ProviderRelayURL is the controller endpoint that relays a slot's model
// requests for a Task.
func ProviderRelayURL(controllerURL, namespace, taskName, slot string) string {
	return strings.TrimRight(controllerURL, "/") + "/internal/v1/tasks/" + url.PathEscape(namespace) + "/" +
		url.PathEscape(taskName) + "/providers/" + url.PathEscape(slot)
}
