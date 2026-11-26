/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package openai

import (
	"context"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// transportAuthenticated selects openai-go's Azure token-credential mode for
// a client whose HTTP transport authenticates each request itself: the
// origin-pinned bearer client, which sets the Provider's token, or the
// controller relay, which sets the worker's. It holds and fetches no token;
// the transport replaces the empty bearer the SDK writes.
type transportAuthenticated struct{}

var _ azcore.TokenCredential = transportAuthenticated{}

func (transportAuthenticated) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{ExpiresOn: time.Now().Add(24 * time.Hour)}, nil
}
