package api

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A request rejected before its handler reads the streamed body must not
// leave body bytes on a keep-alive connection, where the server would parse
// them as the next request.
func TestUnreadRequestBodyCannotDesyncKeepAliveConnection(t *testing.T) {
	server := NewServer(fake.NewClientBuilder().Build(), nil, ServerConfig{
		WatchNamespace: "orka-system", EnforceNamespaceIsolation: true, Clientset: kubefake.NewClientset(),
	})
	addr := strings.TrimPrefix(serveCompatTestApp(t, server.app), "http://")

	for _, size := range []int{1 << 10, 12 << 10, 16 << 10, 64 << 10, 300 << 10, 2 << 20} {
		t.Run(fmt.Sprintf("%dB", size), func(t *testing.T) {
			conn := dialTestServer(t, addr)
			// The leftover body is itself a request line, so a desync would
			// surface as a response to it rather than to the health check.
			smuggled := "GET /smuggled HTTP/1.1\r\nHost: x\r\n\r\n"
			body := strings.Repeat("x", size-len(smuggled)) + smuggled
			// Write in the background: the server may answer and close
			// before a large body has been sent.
			go func() {
				_, _ = fmt.Fprintf(conn, "PUT /api/v1/security/repositories/r/threat-model HTTP/1.1\r\nHost: x\r\n"+
					"Content-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
			}()

			reader := bufio.NewReader(conn)
			first, err := http.ReadResponse(reader, nil)
			if err != nil && size > maxDrainedRequestBodyBytes {
				// Closed mid-upload: the remainder can no longer be parsed.
				require.True(t, errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET), "unexpected read error: %v", err)
				return
			}
			require.NoError(t, err)
			_, _ = io.Copy(io.Discard, first.Body)
			_ = first.Body.Close()
			require.Equal(t, http.StatusUnauthorized, first.StatusCode)
			require.Equal(t, size > maxDrainedRequestBodyBytes, first.Close, "only an oversized remainder may cost the connection")
			if first.Close {
				return
			}
			requireNextRequestServed(t, conn, reader, "/healthz")
		})
	}
}

func TestDrainUnreadRequestBody(t *testing.T) {
	app := fiber.New(fiber.Config{BodyLimit: defaultAPIRequestBodyLimit, StreamRequestBody: true})
	app.Use(drainUnreadRequestBody)
	app.Post("/ignore", func(c fiber.Ctx) error { return c.SendStatus(http.StatusAccepted) })
	app.Post("/bind", func(c fiber.Ctx) error {
		var payload map[string]string
		if err := c.Bind().JSON(&payload); err != nil {
			return err
		}
		return c.SendStatus(http.StatusCreated)
	})
	app.Get("/ok", func(c fiber.Ctx) error { return c.SendString("ok") })
	addr := strings.TrimPrefix(serveCompatTestApp(t, app), "http://")

	t.Run("requests without a body keep the connection", func(t *testing.T) {
		conn := dialTestServer(t, addr)
		reader := bufio.NewReader(conn)
		requireNextRequestServed(t, conn, reader, "/ok")
		requireNextRequestServed(t, conn, reader, "/ok")
	})

	cases := []struct {
		name      string
		path      string
		body      string
		status    int
		wantClose bool
	}{
		{"empty body keeps the connection", "/ignore", "", http.StatusAccepted, false},
		{"small ignored body is drained", "/ignore", strings.Repeat("y", 1<<10), http.StatusAccepted, false},
		{"streamed ignored body is drained", "/ignore", strings.Repeat("y", 12<<10), http.StatusAccepted, false},
		{"ignored body at the drain limit is drained", "/ignore", strings.Repeat("y", maxDrainedRequestBodyBytes), http.StatusAccepted, false},
		{"ignored body past the drain limit closes", "/ignore", strings.Repeat("y", maxDrainedRequestBodyBytes+1), http.StatusAccepted, true},
		{"bound body keeps the connection", "/bind", `{"pad":"` + strings.Repeat("y", 2<<20) + `"}`, http.StatusCreated, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := dialTestServer(t, addr)
			go func() {
				_, _ = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", tc.path, len(tc.body), tc.body)
			}()
			reader := bufio.NewReader(conn)
			first, err := http.ReadResponse(reader, nil)
			require.NoError(t, err)
			_ = first.Body.Close()
			require.Equal(t, tc.status, first.StatusCode)
			require.Equal(t, tc.wantClose, first.Close)
			if !tc.wantClose {
				requireNextRequestServed(t, conn, reader, "/ok")
			}
		})
	}
}

func TestDrainUnreadRequestBodyGivesUpOnAStalledClient(t *testing.T) {
	app := fiber.New(fiber.Config{BodyLimit: defaultAPIRequestBodyLimit, StreamRequestBody: true})
	app.Use(drainUnreadRequestBody)
	app.Post("/ignore", func(c fiber.Ctx) error { return c.SendStatus(http.StatusForbidden) })
	addr := strings.TrimPrefix(serveCompatTestApp(t, app), "http://")
	conn := dialTestServer(t, addr)

	// Announce far more than is sent, then stall: the response must still
	// arrive, and the connection must not wait for the rest.
	_, err := fmt.Fprintf(conn, "POST /ignore HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n%s", 64<<10, strings.Repeat("y", 16<<10))
	require.NoError(t, err)
	start := time.Now()
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	_ = response.Body.Close()
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.True(t, response.Close)
	require.Less(t, time.Since(start), requestBodyDrainTimeout+2*time.Second)
}

func dialTestServer(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(20*time.Second)))
	return conn
}

func requireNextRequestServed(t *testing.T, conn net.Conn, reader *bufio.Reader, path string) {
	t.Helper()
	_, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path)
	require.NoError(t, err)
	next, err := http.ReadResponse(reader, nil)
	require.NoError(t, err, "the server kept the connection open, so it must answer the next request")
	_, _ = io.Copy(io.Discard, next.Body)
	_ = next.Body.Close()
	require.Equal(t, http.StatusOK, next.StatusCode, "leftover body bytes were parsed as a request")
	require.False(t, next.Close, "a request without a body must keep the connection")
}

// A request the server cannot parse must not have its headers (credentials
// included) reflected in the error body.
func TestMalformedRequestErrorDoesNotEchoHeaders(t *testing.T) {
	server := NewServer(fake.NewClientBuilder().Build(), nil, ServerConfig{
		WatchNamespace: "orka-system", EnforceNamespaceIsolation: true, Clientset: kubefake.NewClientset(),
	})
	addr := strings.TrimPrefix(serveCompatTestApp(t, server.app), "http://")
	conn := dialTestServer(t, addr)
	_, err := fmt.Fprint(conn, "GET bad HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer reflected-secret\r\nCookie: session=reflected-cookie\r\n\r\n")
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	_ = response.Body.Close()
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.NotContains(t, string(body), "reflected-secret")
	require.NotContains(t, string(body), "reflected-cookie")
}
