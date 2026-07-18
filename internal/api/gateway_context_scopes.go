/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

const (
	// ContextScopeGatewaysRead authorizes gateway resource and ledger reads.
	ContextScopeGatewaysRead = "orka:gateways:read"
	// ContextScopeGatewaysOperate authorizes dead-lettered delivery retries.
	ContextScopeGatewaysOperate = "orka:gateways:operate"
)
