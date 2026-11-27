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

type transportAuthenticated struct{}

var _ azcore.TokenCredential = transportAuthenticated{}

func (transportAuthenticated) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{ExpiresOn: time.Now().Add(24 * time.Hour)}, nil
}
