package source

import "errors"

// IsPacketContentRejection permits a caller to omit optional context and retry
// its still-required selection. Every retry must repeat normal provenance and
// content checks; identity, transport and rate-limit failures are not covered.
func IsPacketContentRejection(err error) bool {
	return errors.Is(err, errPacketLimit) || errors.Is(err, errPacketSecret)
}
