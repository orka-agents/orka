package storageclient

import "bytes"

// ConnectorCommitAttempts bounds the rereads of one connector write.
const ConnectorCommitAttempts = connectorCommitAttempts

// ObserveConnectorDataKeys reports a copy of every connector data key the
// client generates to observe until the returned function restores the
// default. Tests use it to prove a provider never receives one.
func ObserveConnectorDataKeys(observe func(key []byte)) (restore func()) {
	previous := newConnectorDataKey
	newConnectorDataKey = func() ([]byte, error) {
		key, err := previous()
		if err == nil {
			observe(bytes.Clone(key))
		}
		return key, err
	}
	return func() { newConnectorDataKey = previous }
}
