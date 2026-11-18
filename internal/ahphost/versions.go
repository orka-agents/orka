/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"encoding/json"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// compareVersions compares two MAJOR.MINOR.PATCH versions. Malformed versions
// compare as newer than anything, so unknown input is never treated as known.
func compareVersions(a, b string) int {
	pa, okA := parseVersion(a)
	pb, okB := parseVersion(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return 1
	case !okB:
		return -1
	}
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVersion(version string) ([3]uint64, bool) {
	var parts [3]uint64
	index, digits := 0, 0
	for i := 0; i < len(version); i++ {
		c := version[i]
		switch {
		case c == '.':
			if digits == 0 || index == 2 {
				return parts, false
			}
			index++
			digits = 0
		case c >= '0' && c <= '9':
			if digits > 0 && parts[index] == 0 {
				return parts, false // leading zero
			}
			parts[index] = parts[index]*10 + uint64(c-'0')
			digits++
			if digits > 9 {
				return parts, false
			}
		default:
			return parts, false
		}
	}
	return parts, index == 2 && digits > 0
}

// actionKnownTo reports whether a client that negotiated version knows the action type.
func actionKnownTo(actionType ahptypes.ActionType, version string) bool {
	introduced, ok := actionIntroducedIn[actionType]
	return ok && compareVersions(introduced, version) <= 0
}

// notificationKnownTo reports whether a client that negotiated version knows the notification.
func notificationKnownTo(method, version string) bool {
	introduced, ok := notificationIntroducedIn[method]
	return ok && compareVersions(introduced, version) <= 0
}

// isClientDispatchable reports whether a client may dispatch the action type.
func isClientDispatchable(actionType ahptypes.ActionType) bool {
	return clientDispatchable[actionType]
}

// actionType reads the type discriminator of an action.
func actionType(action ahptypes.StateAction) (ahptypes.ActionType, error) {
	raw, err := json.Marshal(action)
	if err != nil {
		return "", err
	}
	return rawActionType(raw)
}

func rawActionType(raw json.RawMessage) (ahptypes.ActionType, error) {
	var probe struct {
		Type ahptypes.ActionType `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", err
	}
	return probe.Type, nil
}
