package patchverification

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPreparationReportDistinguishesIncompleteTransportFromInvalidEvidence(t *testing.T) {
	for _, body := range []string{"", "{", `{"version":1,`} {
		_, err := DecodePreparationReport(strings.NewReader(body))
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	}
	_, err := DecodePreparationReport(strings.NewReader(`{"unrecognized":true}`))
	require.Error(t, err)
	require.NotErrorIs(t, err, io.ErrUnexpectedEOF)
}
