/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"strings"
	"testing"
)

func TestConnectorPrivateEndpointsPermitted(t *testing.T) {
	if ok, err := connectorPrivateEndpointsPermitted("", "https://orka.example.com"); ok || err != nil {
		t.Fatalf("empty acknowledgement = %t %v", ok, err)
	}
	ack := ConnectorPrivateEndpointsAcknowledgement
	if ok, err := connectorPrivateEndpointsPermitted(ack, "http://localhost:18080"); !ok || err != nil {
		t.Fatalf("local fixture = %t %v", ok, err)
	}
	if ok, err := connectorPrivateEndpointsPermitted(ack, "http://[::1]:18080"); !ok || err != nil {
		t.Fatalf("IPv6 loopback: ok = %t err = %v", ok, err)
	}
	if ok, err := connectorPrivateEndpointsPermitted(ack, "http://127.0.0.1:18080/"); !ok || err != nil {
		t.Fatalf("loopback fixture = %t %v", ok, err)
	}
	for name, tc := range map[string]struct{ ack, base, want string }{
		"true is not enough":  {"true", "http://localhost:18080", "accepts only the literal"},
		"wrong literal":       {"yes-really", "http://localhost:18080", "accepts only the literal"},
		"production callback": {ack, "https://orka.example.com", "local fixtures only"},
		"http but public":     {ack, "http://orka.example.com", "local fixtures only"},
		"no callback":         {ack, "", "local fixtures only"},
	} {
		ok, err := connectorPrivateEndpointsPermitted(tc.ack, tc.base)
		if ok || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: ok=%t err=%v", name, ok, err)
		}
	}
}
