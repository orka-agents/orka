//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

var errObserverReported = errors.New("application observer reported incomplete execution")

func targetConfig(explicitAddress string) (*rest.Config, error) {
	server := explicitAddress
	if server == "" {
		server = os.Getenv("ORKA_APPLICATION_API_SERVER")
	}
	if server == "" {
		// Accept only the credential-free, fixed-file kubeconfig emitted by the
		// backend. Never execute kubeconfig plugins or accept inline credentials.
		var err error
		server, err = targetServerFromKubeconfig()
		if err != nil {
			return nil, err
		}
	}
	endpoint, err := url.Parse(server)
	if err != nil || endpoint.Scheme != "https" || net.ParseIP(endpoint.Hostname()) == nil ||
		endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("application API server must be the fixed literal isolated target")
	}
	config := &rest.Config{
		Host: server, TLSClientConfig: rest.TLSClientConfig{CAFile: "/target/ca.crt"},
		BearerTokenFile: targetTokenPath, UserAgent: "orka-application-observer",
		QPS: 5, Burst: 10, Timeout: 10 * time.Second,
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
	}
	config.WrapTransport = func(transport http.RoundTripper) http.RoundTripper {
		return &targetTransport{host: endpoint.Host, base: transport}
	}
	return config, nil
}

func targetServerFromKubeconfig() (string, error) {
	raw, err := clientcmd.LoadFromFile(pv.ApplicationKubeconfigPath)
	if err != nil || len(raw.Clusters) != 1 || len(raw.AuthInfos) != 1 || len(raw.Contexts) != 1 {
		return "", errors.New("application target configuration is unavailable")
	}
	server := ""
	for _, cluster := range raw.Clusters {
		if cluster.InsecureSkipTLSVerify || cluster.CertificateAuthority != "/target/ca.crt" ||
			len(cluster.CertificateAuthorityData) != 0 || cluster.ProxyURL != "" || cluster.TLSServerName != "" {
			return "", pv.ErrEvidence
		}
		server = cluster.Server
	}
	for _, auth := range raw.AuthInfos {
		if !targetAuthenticationFilesOnly(auth) {
			return "", pv.ErrEvidence
		}
	}
	return server, nil
}

func targetAuthenticationFilesOnly(auth *clientcmdapi.AuthInfo) bool {
	return auth.TokenFile == targetTokenPath && auth.Token == "" && auth.Username == "" &&
		auth.Password == "" && auth.Exec == nil && auth.AuthProvider == nil &&
		auth.ClientKey == "" && auth.ClientCertificate == "" &&
		len(auth.ClientKeyData) == 0 && len(auth.ClientCertificateData) == 0 &&
		auth.Impersonate == "" && len(auth.ImpersonateGroups) == 0 && len(auth.ImpersonateUserExtra) == 0
}

type targetTransport struct {
	host string
	base http.RoundTripper
}

func (transport *targetTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != transport.host || request.URL.User != nil {
		return nil, errors.New("application observer cannot contact a different API endpoint")
	}
	return transport.base.RoundTrip(request)
}

func runObserve(ctx context.Context, input pv.ApplicationWorkerInput, output io.Writer) error {
	if _, err := measureProcess(); err != nil {
		return err
	}
	if input.Artifact == nil || input.Subject == nil || input.CheckID == "" {
		return pv.ErrEvidence
	}
	inputDigest, err := pv.ApplicationWorkerInputDigest(input)
	if err != nil {
		return err
	}
	config, err := targetConfig("")
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return pv.ErrEvidence
	}
	resources, err := dynamic.NewForConfig(config)
	if err != nil {
		return pv.ErrEvidence
	}
	containerName := os.Getenv("ORKA_APPLICATION_CONTAINER_NAME")
	if containerName == "" {
		containerName = input.Worker.ContainerName
	}
	workerTarget := input.Target
	if input.Worker.Namespace != "" {
		workerTarget.Namespace, workerTarget.NamespaceUID = input.Worker.Namespace, input.Worker.NamespaceUID
	}
	identity, err := pv.ReadApplicationContainerIdentity(ctx, kube, workerTarget,
		os.Getenv("ORKA_APPLICATION_POD_NAME"), os.Getenv("ORKA_APPLICATION_POD_UID"), containerName)
	if err != nil {
		return err
	}
	if input.Worker.PodUID != "" && input.Worker.PodUID != identity.PodUID ||
		input.Worker.ContainerID != "" && input.Worker.ContainerID != identity.ContainerID ||
		input.Worker.ImageID != "" && input.Worker.ImageID != identity.ImageID {
		return pv.ErrBinding
	}
	input.Worker = identity
	if pv.ValidateApplicationWorkerInput(input) != nil {
		return pv.ErrBinding
	}
	var marker []byte
	for _, check := range input.Manifest.Checks {
		if check.ID == input.CheckID && check.Application.TCP != nil {
			if input.MarkerRef != nil {
				marker, err = readProtectedMarker(ctx, kube, input)
			} else {
				marker, err = readMarker()
			}
			if err != nil {
				return err
			}
		}
	}
	clients, cleanup, err := actorClients(ctx, input, config)
	if err != nil {
		return err
	}
	defer cleanup()
	frame := func(kind string) pv.ApplicationStreamFrame {
		return pv.ApplicationStreamFrame{Version: pv.ApplicationProtocolVersion, Kind: kind, TaskUID: input.TaskUID,
			PodUID: identity.PodUID, InputDigest: inputDigest}
	}
	onAction := func(action pv.ApplicationActionObservation) error {
		next := frame(pv.ApplicationFrameAction)
		next.Action = &action
		return writeReport(output, next)
	}
	evidence, err := pv.ObserveApplicationWithActors(ctx, input, kube, resources, marker, clients, onAction)
	if err != nil || ctx.Err() != nil {
		failed := frame(pv.ApplicationFrameFailure)
		failed.Code = "observer-incomplete"
		_ = writeReport(output, failed)
		return errObserverReported
	}
	final := frame(pv.ApplicationFrameFinal)
	final.Report = &pv.ApplicationReport{Version: pv.ApplicationProtocolVersion,
		TaskUID: input.TaskUID, PodUID: identity.PodUID, Evidence: evidence}
	if err := writeReport(output, final); err != nil {
		return errObserverReported
	}
	return nil
}

func readProtectedMarker(
	ctx context.Context, kube kubernetes.Interface, input pv.ApplicationWorkerInput,
) ([]byte, error) {
	ref := input.MarkerRef
	if ref == nil || ref.Namespace == input.Target.Namespace || ref.NamespaceUID == input.Target.NamespaceUID ||
		ref.Digest != input.MarkerDigest {
		return nil, pv.ErrBinding
	}
	secret, err := kube.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil || secret == nil || string(secret.UID) != ref.UID || secret.DeletionTimestamp != nil ||
		secret.Immutable == nil || !*secret.Immutable {
		return nil, pv.ErrBinding
	}
	marker, ok := secret.Data[ref.Key]
	if !ok || len(marker) < 32 || len(marker) > 128 || pv.Digest(marker) != ref.Digest {
		return nil, pv.ErrBinding
	}
	return marker, nil
}
