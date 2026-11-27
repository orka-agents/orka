package adapterkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
)

const (
	DefaultAddress              = ":8443"
	DefaultProbeAddress         = ":8080"
	DefaultSettleTimeout        = 10 * time.Second
	DefaultReloadInterval       = 10 * time.Second
	DefaultReadinessInterval    = 10 * time.Second
	DefaultShutdownTimeout      = 30 * time.Second
	DefaultMaxConcurrentStreams = 256
)

func DefaultLimits() wire.Limits {
	return wire.Limits{
		MaxConcurrent: map[storagev1.CallClass]int{
			storagev1.CallControl: 8, storagev1.CallInteractive: 128, storagev1.CallBackground: 16, storagev1.CallBulk: 16,
		},
		MaxDuration: map[storagev1.CallClass]time.Duration{
			storagev1.CallControl: 10 * time.Second, storagev1.CallInteractive: time.Minute,
			storagev1.CallBackground: 5 * time.Minute, storagev1.CallBulk: 5 * time.Minute,
		},
	}
}

var limitedClasses = []storagev1.CallClass{
	storagev1.CallControl, storagev1.CallInteractive, storagev1.CallBackground, storagev1.CallBulk,
}

type Config struct {
	State          storagev1.StateProvider
	Artifacts      storagev1.ArtifactProvider
	AdapterName    string
	AdapterVersion string

	Address       string
	Listener      net.Listener
	ProbeAddress  string
	ProbeListener net.Listener

	CertFile string
	KeyFile  string

	BearerTokenDir        string
	ClientCAFile          string
	AllowedClientURIs     []string
	AllowedClientDNSNames []string

	Limits               wire.Limits
	MaxBlobBytes         int64
	MaxConcurrentStreams uint32
	SettleTimeout        time.Duration
	ReloadInterval       time.Duration
	ReadinessInterval    time.Duration
	ShutdownTimeout      time.Duration
	Logger               *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.Address == "" {
		c.Address = DefaultAddress
	}
	if c.ProbeAddress == "" {
		c.ProbeAddress = DefaultProbeAddress
	}
	defaults := DefaultLimits()
	limits := wire.Limits{MaxConcurrent: map[storagev1.CallClass]int{}, MaxDuration: map[storagev1.CallClass]time.Duration{}}
	for _, class := range limitedClasses {
		limits.MaxConcurrent[class] = defaults.MaxConcurrent[class]
		if value := c.Limits.MaxConcurrent[class]; value > 0 {
			limits.MaxConcurrent[class] = value
		}
		limits.MaxDuration[class] = defaults.MaxDuration[class]
		if value := c.Limits.MaxDuration[class]; value > 0 {
			limits.MaxDuration[class] = value
		}
	}
	c.Limits = limits
	if c.MaxConcurrentStreams == 0 {
		c.MaxConcurrentStreams = DefaultMaxConcurrentStreams
	}
	if c.SettleTimeout <= 0 {
		c.SettleTimeout = DefaultSettleTimeout
	}
	if c.ReloadInterval <= 0 {
		c.ReloadInterval = DefaultReloadInterval
	}
	if c.ReadinessInterval <= 0 {
		c.ReadinessInterval = DefaultReadinessInterval
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

func (c *Config) validate() error {
	var problems []error
	if c.State == nil && c.Artifacts == nil {
		problems = append(problems, errors.New("a state or artifacts provider is required"))
	}
	if c.CertFile == "" || c.KeyFile == "" {
		problems = append(problems, errors.New("a serving certificate and key are required: storage traffic uses TLS"))
	}
	if c.BearerTokenDir == "" && c.ClientCAFile == "" {
		problems = append(problems, errors.New("client authentication is required: set a bearer token directory, a client CA, or both"))
	}
	if c.ClientCAFile != "" && len(c.AllowedClientURIs) == 0 && len(c.AllowedClientDNSNames) == 0 {
		problems = append(problems, errors.New("mTLS authentication requires at least one allowed client URI or DNS name"))
	}
	if c.ClientCAFile == "" && (len(c.AllowedClientURIs) > 0 || len(c.AllowedClientDNSNames) > 0) {
		problems = append(problems, errors.New("allowed client names need a client CA"))
	}
	return errors.Join(problems...)
}

func Serve(ctx context.Context, config Config) error {
	serving := false
	defer func() {
		if !serving {
			closeListeners(config.Listener, config.ProbeListener)
		}
	}()
	config.applyDefaults()
	if err := config.validate(); err != nil {
		return fmt.Errorf("storage adapter configuration: %w", err)
	}
	logger := config.Logger
	metrics := newMetrics()
	creds, err := loadCredentials(&config, metrics)
	if err != nil {
		return err
	}
	server, err := wire.NewServer(wire.ServerConfig{
		State:         config.State,
		Artifacts:     config.Artifacts,
		Info:          wire.ServerInfo{AdapterName: config.AdapterName, AdapterVersion: config.AdapterVersion},
		Authenticator: creds,
		Logger:        logger,
		Limits:        config.Limits,
		MaxBlobBytes:  config.MaxBlobBytes,
		Observer:      metrics,
	})
	if err != nil {
		return err
	}
	settle := settleInterceptors(config.SettleTimeout)
	grpcServer := grpc.NewServer(append(server.ServerOptions(),
		grpc.Creds(credentials.NewTLS(creds.serverTLS())),
		grpc.MaxConcurrentStreams(config.MaxConcurrentStreams),
		grpc.ChainUnaryInterceptor(settle.unary),
		grpc.ChainStreamInterceptor(settle.stream),
	)...)
	server.Register(grpcServer)

	rawListener, probeListener, err := listen(&config)
	if err != nil {
		return err
	}
	listener := newTrackedListener(rawListener)
	serving = true
	readiness := &readiness{config: &config, server: server, metrics: metrics, logger: logger}
	probe := &http.Server{
		Handler: probeHandler(readiness, metrics), ReadHeaderTimeout: probeReadTimeout, ReadTimeout: probeReadTimeout,
		WriteTimeout: 10 * time.Second, IdleTimeout: probeIdleTimeout,
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	var workers sync.WaitGroup
	grpcServed := make(chan struct{})
	go func() {
		defer close(grpcServed)
		if err := grpcServer.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			errs <- fmt.Errorf("storage server: %w", err)
		}
	}()
	workers.Go(func() {
		if err := probe.Serve(probeListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("probe server: %w", err)
		}
	})
	workers.Go(func() { creds.watch(runCtx, config.ReloadInterval) })
	workers.Go(func() { readiness.watch(runCtx) })
	logger.InfoContext(ctx, "storage adapter serving", "address", listener.Addr().String(),
		"probeAddress", probeListener.Addr().String(), "adapter", config.AdapterName)

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errs:
	}
	readiness.drain()
	cancel()
	forced := stopGracefully(grpcServer, listener, config.ShutdownTimeout)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelShutdown()
	_ = probe.Shutdown(shutdownCtx)
	workersDone := make(chan struct{})
	go func() {
		workers.Wait()
		close(workersDone)
	}()
	workerWait := config.ShutdownTimeout
	if workerWait <= 0 {
		workerWait = 5 * time.Second
	}
	select {
	case <-workersDone:
	case <-time.After(workerWait):
		forced = true
	}
	if !forced {
		<-grpcServed
	}
	logger.InfoContext(ctx, "storage adapter stopped", "forced", forced)
	return serveErr
}

var (
	probeReadTimeout = 5 * time.Second
	probeIdleTimeout = 60 * time.Second
)

func listen(config *Config) (net.Listener, net.Listener, error) {
	listener := config.Listener
	if listener == nil {
		var err error
		if listener, err = net.Listen("tcp", config.Address); err != nil {
			return nil, nil, fmt.Errorf("listen for storage calls: %w", err)
		}
	}
	probeListener := config.ProbeListener
	if probeListener == nil {
		var err error
		if probeListener, err = net.Listen("tcp", config.ProbeAddress); err != nil {
			if config.Listener == nil {
				_ = listener.Close()
			}
			return nil, nil, fmt.Errorf("listen for probes: %w", err)
		}
	}
	return listener, probeListener, nil
}

func closeListeners(listeners ...net.Listener) {
	for _, listener := range listeners {
		if listener != nil {
			_ = listener.Close()
		}
	}
}

func stopGracefully(server *grpc.Server, listener *trackedListener, timeout time.Duration) bool {
	stopped := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(stopped)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-stopped:
		return false
	case <-timer.C:
		go server.Stop()
		listener.closeAll()
		return true
	}
}

type trackedListener struct {
	net.Listener
	mu     sync.Mutex
	conns  map[*trackedConn]struct{}
	closed bool
}

func newTrackedListener(listener net.Listener) *trackedListener {
	return &trackedListener{Listener: listener, conns: map[*trackedConn]struct{}{}}
}

func (l *trackedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &trackedConn{Conn: conn, listener: l}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	l.conns[tracked] = struct{}{}
	return tracked, nil
}

func (l *trackedListener) closeAll() {
	l.mu.Lock()
	l.closed = true
	conns := make([]*trackedConn, 0, len(l.conns))
	for conn := range l.conns {
		conns = append(conns, conn)
	}
	l.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

type trackedConn struct {
	net.Conn
	listener *trackedListener
}

func (c *trackedConn) Close() error {
	c.listener.mu.Lock()
	delete(c.listener.conns, c)
	c.listener.mu.Unlock()
	return c.Conn.Close()
}

type readiness struct {
	config  *Config
	server  *wire.Server
	metrics *metrics
	logger  *slog.Logger

	mu       sync.Mutex
	ready    bool
	draining bool
}

func (r *readiness) watch(ctx context.Context) {
	ticker := time.NewTicker(r.config.ReadinessInterval)
	defer ticker.Stop()
	for {
		r.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *readiness) check(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, r.config.Limits.MaxDuration[storagev1.CallControl])
	defer cancel()
	err := r.probe(checkCtx)
	r.mu.Lock()
	defer r.mu.Unlock()
	ready := err == nil && !r.draining
	if ready != r.ready {
		if ready {
			r.logger.InfoContext(ctx, "storage adapter ready", "dataset", r.server.DatasetID())
		} else if ctx.Err() == nil && !r.draining {
			r.logger.WarnContext(ctx, "storage adapter not ready", "reason", readinessReason(err))
		}
	}
	r.ready = ready
	r.metrics.setReady(ready)
}

var errProviderPanicked = errors.New("the storage provider panicked")

func readinessReason(err error) string {
	switch {
	case errors.Is(err, errProviderPanicked):
		return "the storage provider panicked"
	case errors.Is(err, context.DeadlineExceeded):
		return "the readiness check timed out"
	case errors.Is(err, storagev1.ErrNotReady):
		return "the storage provider is not ready"
	case errors.Is(err, storagev1.ErrUnavailable):
		return "the storage provider is unavailable"
	case errors.Is(err, storagev1.ErrUnauthenticated):
		return "the storage provider cannot authenticate"
	case errors.Is(err, storagev1.ErrIncompatible):
		return "the storage provider is incompatible"
	default:
		return "the storage provider failed its readiness check"
	}
}

func (r *readiness) probe(ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errProviderPanicked
		}
	}()
	if err := r.server.Refresh(ctx); err != nil {
		return err
	}
	if r.config.State != nil {
		if err := r.config.State.HealthCheck(ctx); err != nil {
			return err
		}
	}
	if r.config.Artifacts != nil {
		return r.config.Artifacts.HealthCheck(ctx)
	}
	return nil
}

func (r *readiness) isReady() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready
}

func (r *readiness) drain() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.draining = true
	r.ready = false
	r.metrics.setReady(false)
}

func probeHandler(readiness *readiness, metrics *metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !readiness.isReady() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, metrics.exposition())
	})
	return mux
}
