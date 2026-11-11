package source

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInventoryResponseLimitUsesActualEncodedBytes(t *testing.T) {
	const limit = MaxInventoryBytes
	for _, size := range []int{limit - 1, limit, limit + 1} {
		api := newAPIFixture(t, map[string]apiReply{
			fixturePrefix: {http.StatusOK, `{"value":"` + strings.Repeat("a", size-len(`{"value":""}`)) + `"}`},
		})
		var data map[string]string
		err := getJSONWithinLimit(t.Context(), api.client.HTTPClient, api.server.URL+fixturePrefix, &data, "inventory", limit)
		if size <= limit {
			require.NoError(t, err)
			raw, err := json.Marshal(data)
			require.NoError(t, err)
			require.Len(t, raw, size)
		} else {
			require.Error(t, err)
			require.Nil(t, data)
		}
	}
}
