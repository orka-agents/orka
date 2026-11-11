package patchverification

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestApplicationCustomResourcePreservesKubernetesNumericValues(t *testing.T) {
	tests := []struct {
		literal string
		value   any
	}{
		{"9007199254740991", int64(9007199254740991)},
		{"9007199254740992", int64(9007199254740992)},
		{"9007199254740993", int64(9007199254740993)},
		{"-9007199254740993", int64(-9007199254740993)},
		{"9223372036854775807", int64(9223372036854775807)},
		{"-9223372036854775808", int64(-9223372036854775808)},
		{"1.5", float64(1.5)},
		{"0.125", float64(0.125)},
		{"-3.75", float64(-3.75)},
	}
	for _, test := range tests {
		t.Run(test.literal, func(t *testing.T) {
			spec := fmt.Sprintf(`{"direct":%s,"nested":{"values":[%s,{"value":%s}],"fraction":1.25}}`,
				test.literal, test.literal, test.literal)
			raw := json.RawMessage(`{"apiVersion":"testing.example/v1","kind":"Widget","metadata":{"name":"numeric"},"spec":` + spec + `}`)
			before := bytes.Clone(raw)
			object, err := ValidateApplicationResourceWithPolicy(raw, applicationTestPolicy(t))
			require.NoError(t, err)
			require.Equal(t, before, []byte(raw), "admission must not rewrite the frozen resource")
			actual := object.Object["spec"].(map[string]any)
			require.IsType(t, test.value, actual["direct"])
			require.Equal(t, test.value, actual["direct"])
			nested := actual["nested"].(map[string]any)
			values := nested["values"].([]any)
			require.Equal(t, test.value, values[0])
			require.Equal(t, test.value, values[1].(map[string]any)["value"])
			require.Equal(t, float64(1.25), nested["fraction"])
			require.NotPanics(t, func() { object.DeepCopy() }, "values must remain Kubernetes-unstructured compatible")

			// Compare serialized spec with a number-preserving view of the
			// frozen JSON, independent of the decoder being tested.
			decoder := json.NewDecoder(bytes.NewReader([]byte(spec)))
			decoder.UseNumber()
			var expected map[string]any
			require.NoError(t, decoder.Decode(&expected))
			want, err := json.Marshal(expected)
			require.NoError(t, err)
			got, err := json.Marshal(actual)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

func TestApplicationBuiltinIntegerPortsRemainCompatibleWithTypedResources(t *testing.T) {
	raw := json.RawMessage(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"subject-service"},"spec":{"type":"ClusterIP","selector":{"orka.ai/application-target":"subject"},"ports":[{"port":8080,"targetPort":8081,"protocol":"TCP"}]}}`)
	for _, policy := range []*ApplicationPolicy{nil, applicationTestPolicy(t)} {
		object, err := ValidateApplicationResourceWithPolicy(raw, policy)
		require.NoError(t, err)
		port := object.Object["spec"].(map[string]any)["ports"].([]any)[0].(map[string]any)
		require.Equal(t, int64(8080), port["port"])
		require.Equal(t, int64(8081), port["targetPort"])
		var service corev1.Service
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &service))
		require.Equal(t, int32(8080), service.Spec.Ports[0].Port)
		require.Equal(t, 8081, service.Spec.Ports[0].TargetPort.IntValue())
	}
	for _, invalid := range []any{int64(1023), int64(65536), float64(8080.5), "8080", nil} {
		require.False(t, applicationPort(invalid))
	}
	require.True(t, applicationPort(int64(8080)))
	require.True(t, applicationPort(float64(8080)))
}
