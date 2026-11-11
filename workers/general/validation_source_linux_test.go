//go:build linux

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidationPreparationCannotRunInOrdinaryWorkerOrHost(t *testing.T) {
	require.ErrorIs(t, validationPreparationPreflight(), errValidationPreparation)
	require.ErrorIs(t, runBoundedValidationGit([]string{"/usr/bin/git", "version"}), errValidationPreparation)
	require.ErrorIs(t, runBoundedValidationGit([]string{"/bin/sh", "-c", "exit 0"}), errValidationPreparation)
	for _, args := range [][]string{nil, {"/workspace/other"}, {"/input/request.json", "unexpected"}} {
		require.ErrorIs(t, runValidationSourcePreparation(context.Background(), args), errValidationPreparation)
	}
	require.NotContains(t, strings.ToLower(errValidationPreparation.Error()), "token")
}
