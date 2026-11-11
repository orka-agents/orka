//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"k8s.io/client-go/kubernetes"
)

func readMarker() ([]byte, error) {
	file, err := openProjected(pv.ApplicationMarkerPath)
	if err != nil {
		return nil, err
	}
	marker, readErr := io.ReadAll(io.LimitReader(file, 129))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(marker) < 32 || len(marker) > 128 {
		return nil, errors.New("application protected marker is unavailable")
	}
	return marker, nil
}

func runMarker(ctx context.Context, port int) error {
	if port < 1024 || port > 65535 {
		return pv.ErrEvidence
	}
	if _, err := measureProcess(); err != nil {
		return err
	}
	if err := noTargetCredentials(); err != nil {
		return err
	}
	marker, err := readMarker()
	if err != nil {
		return err
	}
	return serveMarker(ctx, port, marker)
}

func runMarkerFromInput(ctx context.Context, input pv.ApplicationWorkerInput) error {
	if _, err := measureProcess(); err != nil {
		return err
	}
	if input.MarkerRef == nil || input.FixtureID == "" {
		return pv.ErrEvidence
	}
	port := 0
	for _, fixture := range input.Manifest.Application.Fixtures {
		if fixture.ID == input.FixtureID && (fixture.Kind == "" || fixture.Kind == pv.ApplicationFixtureMarker) {
			port = fixture.Port
		}
	}
	if port == 0 {
		return pv.ErrBinding
	}
	config, err := targetConfig("")
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return pv.ErrEvidence
	}
	marker, err := readProtectedMarker(ctx, kube, input)
	if err != nil {
		return err
	}
	return serveMarker(ctx, port, marker)
}

func serveMarker(ctx context.Context, port int, marker []byte) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		return errors.New("application protected fixture could not bind")
	}
	defer func() { _ = listener.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	available := make(chan struct{}, 16)
	var connections sync.WaitGroup
	defer connections.Wait()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("application fixture listener failed")
		}
		select {
		case available <- struct{}{}:
		default:
			_ = connection.Close()
			continue
		}
		connections.Go(func() {
			defer func() { <-available }()
			defer func() { _ = connection.Close() }()
			if connection.SetDeadline(time.Now().Add(5*time.Second)) != nil {
				return
			}
			// The EOF-terminated marker is only network traffic to the subject or
			// protected observer; no raw marker is ever written to Pod logs.
			_, _ = connection.Write(append(append([]byte{}, marker...), '\n'))
		})
	}
}
