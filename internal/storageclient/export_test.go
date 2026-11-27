package storageclient

import "bytes"

const ConnectorCommitAttempts = connectorCommitAttempts

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
