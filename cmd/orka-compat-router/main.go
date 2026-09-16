/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/yaml"

	"github.com/orka-agents/orka/internal/api"
)

func main() {
	listenAddress := flag.String("listen-address", ":8080", "HTTP listen address")
	routesFile := flag.String("routes-file", "", "YAML file mapping namespaces to Orka API origins")
	flag.Parse()
	log.SetLogger(zap.New())
	logger := log.Log.WithName("compat-router")
	if err := run(*listenAddress, *routesFile); err != nil {
		logger.Error(err, "compatibility router stopped")
		os.Exit(1)
	}
}

func loadRoutes(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("routes-file is required and must be readable")
	}
	var config struct {
		Namespaces map[string]string `json:"namespaces"`
	}
	if err := yaml.UnmarshalStrict(data, &config); err != nil {
		// Parser diagnostics may reproduce configuration values. Keep them out
		// of logs, including accidentally supplied credentials.
		return nil, fmt.Errorf("routes-file must contain a YAML namespaces mapping")
	}
	return config.Namespaces, nil
}

func run(address, routesFile string) error {
	namespaces, err := loadRoutes(routesFile)
	if err != nil {
		return err
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster Kubernetes configuration is required")
	}
	scheme := runtime.NewScheme()
	if err := authenticationv1.AddToScheme(scheme); err != nil {
		return err
	}
	kubeClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("cannot initialize TokenReview client")
	}
	router, err := api.NewCompatRouter(kubeClient, namespaces)
	if err != nil {
		return err
	}
	defer router.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server := &http.Server{
		Addr: address, Handler: router,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       time.Minute,
		// Installations own chat deadlines. Do not truncate long JSON responses
		// or SSE streams with a shared listener write timeout.
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	log.FromContext(ctx).Info("compatibility router listening", "address", address, "namespaces", len(namespaces))
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return server.Close()
		}
	}
	return nil
}
