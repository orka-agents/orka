package adapterkit

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/orka-agents/orka/pkg/storage/v1/wire"
)

// stalledWriter is a response writer whose client stops reading.
type stalledWriter struct {
	header  http.Header
	writing chan struct{}
	release chan struct{}
}

func (w *stalledWriter) Header() http.Header { return w.header }
func (w *stalledWriter) WriteHeader(int)     {}
func (w *stalledWriter) Write(data []byte) (int, error) {
	close(w.writing)
	<-w.release
	return len(data), nil
}

// A scrape that stalls while its response is written never delays the calls
// that record metrics.
func TestStalledScrapesDoNotBlockCalls(t *testing.T) {
	m := newMetrics()
	handler := probeHandler(&readiness{metrics: m}, m)
	writer := &stalledWriter{header: http.Header{}, writing: make(chan struct{}), release: make(chan struct{})}
	defer close(writer.release)
	request, err := http.NewRequest(http.MethodGet, "/metrics", nil)
	require.NoError(t, err)
	go handler.ServeHTTP(writer, request)
	<-writer.writing

	observed := make(chan struct{})
	go func() {
		m.ObserveCall(wire.CallInfo{Method: "GetArtifact", Code: codes.OK, Duration: time.Millisecond})
		m.credentialReload("certificate", true)
		m.setReady(true)
		close(observed)
	}()
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("recording metrics waited for a stalled scrape")
	}
}
