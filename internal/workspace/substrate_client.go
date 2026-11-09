package workspace

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"
)

// SubstrateActorKey is the routable, namespace-qualified identity of an Actor.
// Native Actor names and Atespaces are each DNS labels, so the separator is
// unambiguous. Resource UIDs, rather than this name, fence an Actor lifetime.
func SubstrateActorKey(atespace, name string) string {
	name = strings.TrimSpace(name)
	if strings.Contains(name, ".") {
		return name
	}
	atespace = strings.TrimSpace(atespace)
	if atespace == "" {
		return name
	}
	return name + "." + atespace
}

func substrateObjectRef(actorID, defaultAtespace string) (*ateapipb.ObjectRef, error) {
	name, atespace, qualified := strings.Cut(strings.TrimSpace(actorID), ".")
	if !qualified {
		atespace = strings.TrimSpace(defaultAtespace)
	}
	if len(validation.IsDNS1123Label(name)) != 0 || len(validation.IsDNS1123Label(atespace)) != 0 {
		return nil, NewError("resolve actor", ErrorKindInvalidArgument, "Substrate Actor requires a DNS-label name and explicit Atespace (name.atespace)", false, nil)
	}
	return &ateapipb.ObjectRef{Atespace: atespace, Name: name}, nil
}

type grpcSubstrateControlClient struct {
	conn     *grpc.ClientConn
	client   ateapipb.ControlClient
	atespace string
}

func newGRPCSubstrateControlClient(cfg SubstrateConfig) (*grpcSubstrateControlClient, error) {
	conn, err := newSubstrateConnection(cfg, true)
	if err != nil {
		return nil, err
	}
	return &grpcSubstrateControlClient{conn: conn, client: ateapipb.NewControlClient(conn), atespace: strings.TrimSpace(cfg.Atespace)}, nil
}

func newSubstrateConnection(cfg SubstrateConfig, controlAuth bool) (*grpc.ClientConn, error) {
	if strings.TrimSpace(cfg.APIEndpoint) == "" {
		return nil, NewError("configure substrate", ErrorKindInvalidArgument, "API endpoint is required", false, nil)
	}
	transportCredentials, err := substrateTransportCredentials(cfg)
	if err != nil {
		return nil, err
	}
	options := []grpc.DialOption{grpc.WithTransportCredentials(transportCredentials), grpc.WithUnaryInterceptor(substrateRPCDeadline)}
	if controlAuth {
		mtls := cfg.APICertFile != "" && cfg.APIKeyFile != ""
		bearer := cfg.APIBearerTokenFile != ""
		if mtls == bearer {
			return nil, NewError("configure substrate", ErrorKindInvalidArgument, "Substrate control authentication requires either a client certificate/key pair or a bearer token file", false, nil)
		}
		if bearer {
			creds := substrateBearerCredentials{path: cfg.APIBearerTokenFile}
			if _, err := creds.GetRequestMetadata(context.Background()); err != nil {
				return nil, NewError("configure substrate", ErrorKindInvalidArgument, "cannot read Substrate control bearer token file", false, err)
			}
			options = append(options, grpc.WithPerRPCCredentials(creds))
		}
	}
	conn, err := grpc.NewClient(cfg.APIEndpoint, options...)
	if err != nil {
		return nil, NewError("configure substrate", ErrorKindUnknown, "failed to create Substrate API client", false, err)
	}
	return conn, nil
}

// Native lifecycle RPCs synchronously boot, restore, or checkpoint the Actor.
// Give those operations a bounded window for image pulls and snapshot I/O,
// while keeping discovery and metadata calls short. Caller deadlines and
// cancellation still take precedence, and no failed mutation is replayed.
func substrateRPCDeadline(ctx context.Context, method string, req, reply any, conn *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	timeout := 30 * time.Second
	switch method {
	case ateapipb.Control_ResumeActor_FullMethodName,
		ateapipb.Control_SuspendActor_FullMethodName,
		ateapipb.Control_DeleteActor_FullMethodName,
		ateapipb.Control_DeleteActorTemplate_FullMethodName,
		ateapipb.Control_CreateTag_FullMethodName,
		ateapipb.Control_DeleteTag_FullMethodName:
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return invoke(ctx, method, req, reply, conn, opts...)
}

func (c *grpcSubstrateControlClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func substrateTransportCredentials(cfg SubstrateConfig) (credentials.TransportCredentials, error) {
	tlsConfig, err := substrateTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(tlsConfig), nil
}

// NewSubstrateRouterHTTPClient returns the client for Actor traffic through
// atenet-router. Upstream v1 serves only mTLS from SPIFFE IDs on its ingress
// allowlist, so the pod-certificate client identity is required. Proxies are
// never used for Actor traffic.
func NewSubstrateRouterHTTPClient(cfg SubstrateConfig) (*http.Client, error) {
	router, err := url.Parse(strings.TrimSpace(cfg.RouterURL))
	if err != nil || router.Scheme != "https" || router.Host == "" || router.User != nil {
		return nil, NewError("configure substrate", ErrorKindInvalidArgument, "Substrate router URL must be an https URL", false, nil)
	}
	if cfg.APICertFile == "" || cfg.APIKeyFile == "" {
		return nil, NewError("configure substrate", ErrorKindInvalidArgument, "Substrate router requires a pod-certificate client identity", false, nil)
	}
	tlsConfig, err := substrateTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true, Proxy: nil}
	return &http.Client{Transport: transport}, nil
}

func substrateTLSConfig(cfg SubstrateConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.APIInsecureSkipVerify {
		tlsConfig.InsecureSkipVerify = true //nolint:gosec // explicit local smoke-test option; authentication still required
	} else {
		if strings.TrimSpace(cfg.APICAFile) == "" {
			return nil, NewError("configure substrate", ErrorKindInvalidArgument, "Substrate API trust requires a CA file or insecure skip verify", false, nil)
		}
		data, err := os.ReadFile(cfg.APICAFile)
		if err != nil {
			return nil, NewError("configure substrate", ErrorKindInvalidArgument, "failed to read Substrate API CA file", false, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return nil, NewError("configure substrate", ErrorKindInvalidArgument, "Substrate API CA file has no PEM certificates", false, nil)
		}
		tlsConfig.RootCAs = pool
	}
	if (cfg.APICertFile == "") != (cfg.APIKeyFile == "") {
		return nil, NewError("configure substrate", ErrorKindInvalidArgument, "both Substrate client certificate and key files are required", false, nil)
	}
	if cfg.APICertFile != "" {
		// Kubernetes rotates projected Secret files in place. Reload the pair
		// at every handshake, including reconnects, rather than freezing it at
		// controller startup. Error text never includes certificate contents.
		loadPair := func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			pair, err := tls.LoadX509KeyPair(cfg.APICertFile, cfg.APIKeyFile)
			if err != nil {
				return nil, fmt.Errorf("cannot load Substrate client certificate/key pair")
			}
			return &pair, nil
		}
		if _, err := loadPair(nil); err != nil {
			return nil, NewError("configure substrate", ErrorKindInvalidArgument, "invalid Substrate client certificate/key pair", false, err)
		}
		tlsConfig.GetClientCertificate = loadPair
	}
	return tlsConfig, nil
}

type substrateBearerCredentials struct{ path string }

func (c substrateBearerCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return nil, fmt.Errorf("cannot read Substrate bearer token file")
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, "\r\n\t ") {
		return nil, fmt.Errorf("substrate bearer token file must contain one nonempty token")
	}
	return map[string]string{"authorization": "Bearer " + token}, nil
}

func (substrateBearerCredentials) RequireTransportSecurity() bool { return true }

func (c *grpcSubstrateControlClient) GetActor(ctx context.Context, actorID string) (*substrateActor, error) {
	ref, err := substrateObjectRef(actorID, c.atespace)
	if err != nil {
		return nil, err
	}
	actor, err := c.client.GetActor(ctx, &ateapipb.GetActorRequest{Actor: ref})
	if err != nil {
		return nil, substrateControlError("get actor", err)
	}
	converted := substrateActorFromProto(actor)
	if err := c.resolveBootedTemplateUID(ctx, actor, converted); err != nil {
		return nil, err
	}
	return converted, nil
}

// resolveBootedTemplateUID fills the guest-state template identity of an Actor
// that is placed on a worker but has never been snapshotted. Upstream v1 records
// the template UID only on snapshots; such an Actor booted from the template its
// reference names, and ActorTemplates are immutable, so the named template's
// current UID is its guest-state identity. A template recreated under the same
// name has a new UID and fails the caller's identity comparison.
func (c *grpcSubstrateControlClient) resolveBootedTemplateUID(ctx context.Context, actor *ateapipb.Actor, converted *substrateActor) error {
	if converted == nil || converted.TemplateUID != "" || len(actor.GetStatus().GetSnapshots()) != 0 ||
		actor.GetStatus().GetWorkerAssignment() == nil || actor.GetActorTemplate().GetName() == "" {
		return nil
	}
	template, err := c.client.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{ActorTemplate: actor.GetActorTemplate()})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			// The Actor exists; only its template is gone, which upstream
			// allows. Reporting NotFound would read as a missing Actor: Delete
			// would call a live Actor already deleted and Claim would try to
			// create it. Leave the identity unknown instead. A placed Actor
			// with no template UID fails every comparison against a pinned template UID, and Delete,
			// which is fenced by the Actor UID, can still remove it.
			return nil
		}
		return substrateControlError("get actor template", err)
	}
	converted.TemplateUID = template.GetMetadata().GetUid()
	return nil
}

func (c *grpcSubstrateControlClient) CreateActor(ctx context.Context, actorID, templateNamespace, templateName string) (*substrateActor, error) {
	ref, err := substrateObjectRef(actorID, templateNamespace)
	if err != nil {
		return nil, err
	}
	actor, err := c.client.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: ref.Atespace, Name: ref.Name},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: templateNamespace, Name: templateName},
	}})
	if err != nil {
		return nil, substrateControlError("create actor", err)
	}
	return substrateActorFromProto(actor), nil
}

// ResumeActor resumes an Actor. Upstream v1 removed the per-call boot flag;
// a boot request is honored only when Substrate cannot seed the Actor from a
// golden snapshot: the Actor has no snapshot or source Tag and its template has
// golden snapshots disabled. Otherwise the request fails closed.
func (c *grpcSubstrateControlClient) ResumeActor(ctx context.Context, actorID string, boot bool) (*substrateActor, error) {
	ref, err := substrateObjectRef(actorID, c.atespace)
	if err != nil {
		return nil, err
	}
	if boot {
		if err := c.requireColdBoot(ctx, ref); err != nil {
			return nil, err
		}
	}
	resp, err := c.client.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: ref})
	if err != nil {
		return nil, substrateControlError("resume actor", err)
	}
	return substrateActorFromProto(resp.GetActor()), nil
}

func (c *grpcSubstrateControlClient) requireColdBoot(ctx context.Context, ref *ateapipb.ObjectRef) error {
	actor, err := c.client.GetActor(ctx, &ateapipb.GetActorRequest{Actor: ref})
	if err != nil {
		return substrateControlError("resume actor", err)
	}
	if actor.GetSourceTag().GetName() != "" || len(actor.GetStatus().GetSnapshots()) != 0 {
		return NewError("resume actor", ErrorKindFailedPrecondition, "Substrate Actor already has guest state; boot from scratch is unavailable", false, nil)
	}
	template, err := c.client.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{ActorTemplate: actor.GetActorTemplate()})
	if err != nil {
		return substrateControlError("resume actor", err)
	}
	if template.GetSnapshotConfig().GetGoldenSnapshotConfig().GetMode() != ateapipb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED {
		return NewError("resume actor", ErrorKindFailedPrecondition, "boot from scratch requires an ActorTemplate with golden snapshots disabled", false, nil)
	}
	return nil
}

func (c *grpcSubstrateControlClient) SuspendActor(ctx context.Context, actorID string) (*substrateActor, error) {
	ref, err := substrateObjectRef(actorID, c.atespace)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
		Actor:   ref,
		Options: &ateapipb.SuspendOptions{Mode: ateapipb.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS},
	})
	if err != nil {
		return nil, substrateControlError("suspend actor", err)
	}
	return substrateActorFromProto(resp.GetActor()), nil
}

// DeleteActor terminates the Actor in any state without taking a snapshot.
// A nonempty actorUID is sent as a native precondition, so a different Actor
// recreated under the same name is never deleted. Cleanup callers prove their
// own lifetime and ownership fences before invoking this.
func (c *grpcSubstrateControlClient) DeleteActor(ctx context.Context, actorID, actorUID string) error {
	ref, err := substrateObjectRef(actorID, c.atespace)
	if err != nil {
		return err
	}
	_, err = c.client.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: ref, Options: &ateapipb.DeleteOptions{Uid: strings.TrimSpace(actorUID)}})
	return substrateControlError("delete actor", err)
}

func (c *grpcSubstrateControlClient) ListWorkers(ctx context.Context) ([]substrateWorker, error) {
	var workers []substrateWorker
	pages := substratePages{}
	for {
		resp, err := c.client.ListWorkers(ctx, &ateapipb.ListWorkersRequest{Options: pages.options()})
		if err != nil {
			return nil, substrateControlError("list workers", err)
		}
		for _, worker := range resp.GetWorkers() {
			if worker != nil {
				workers = append(workers, substrateWorkerFromProto(worker))
			}
		}
		more, err := pages.advance(resp.GetNextPageToken())
		if err != nil || !more {
			return workers, err
		}
	}
}

func (c *grpcSubstrateControlClient) ListActors(ctx context.Context) ([]substrateActor, error) {
	var actors []substrateActor
	pages := substratePages{}
	for {
		resp, err := c.client.ListActors(ctx, &ateapipb.ListActorsRequest{Atespace: c.atespace, Options: pages.options()})
		if err != nil {
			return nil, substrateControlError("list actors", err)
		}
		for _, actor := range resp.GetActors() {
			if converted := substrateActorFromProto(actor); converted != nil {
				actors = append(actors, *converted)
			}
		}
		more, err := pages.advance(resp.GetNextPageToken())
		if err != nil || !more {
			return actors, err
		}
	}
}

// A page may be empty while its continuation token is nonempty. A repeated
// token is a protocol error, never an excuse to return incomplete inventory.
type substratePages struct {
	token string
	seen  map[string]struct{}
}

func (p *substratePages) advance(token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	if p.seen == nil {
		p.seen = map[string]struct{}{}
	}
	if _, exists := p.seen[token]; exists {
		return false, NewError("paginate substrate", ErrorKindFailedPrecondition, "Substrate repeated a pagination token; inventory is incomplete", false, nil)
	}
	p.seen[token] = struct{}{}
	p.token = token
	return true, nil
}

func (p *substratePages) options() *ateapipb.ListOptions {
	return &ateapipb.ListOptions{PageSize: 1000, PageToken: p.token}
}

// latestSubstrateSnapshot returns the Actor's newest snapshot generation.
func latestSubstrateSnapshot(snapshots []*ateapipb.Snapshot) *ateapipb.Snapshot {
	var latest *ateapipb.Snapshot
	for _, snapshot := range snapshots {
		if snapshot != nil && (latest == nil || snapshot.GetGeneration() > latest.GetGeneration()) {
			latest = snapshot
		}
	}
	return latest
}

// substrateSnapshotScope classifies a snapshot fidelity. Only VOLUMES holds
// data alone; ROOTFS keeps no process memory but is not data-only either, so it
// is left unclassified and data-only checks fail closed on it.
func substrateSnapshotScope(fidelity ateapipb.SnapshotFidelity) SubstrateSnapshotContentScope {
	switch fidelity {
	case ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES:
		return SubstrateSnapshotContentScopeData
	case ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY:
		return SubstrateSnapshotContentScopeFull
	default:
		return ""
	}
}

func substrateActorFromProto(actor *ateapipb.Actor) *substrateActor {
	if actor == nil {
		return nil
	}
	meta, state := actor.GetMetadata(), actor.GetStatus()
	assignment := state.GetWorkerAssignment()
	converted := &substrateActor{
		ActorID: meta.GetName(), Atespace: meta.GetAtespace(), ActorUID: meta.GetUid(), ActorVersion: meta.GetVersion(),
		TemplateNamespace: actor.GetActorTemplate().GetAtespace(), TemplateName: actor.GetActorTemplate().GetName(),
		Status:       "STATUS_" + strings.TrimPrefix(state.GetState().String(), "ACTOR_STATE_"),
		PodNamespace: assignment.GetWorkerNamespace(), PodName: assignment.GetWorkerPod(),
		PodUID: assignment.GetWorkerPodUid(), PodIP: firstNonEmpty(assignment.GetWorkerPodIps()),
		WorkerName: assignment.GetWorker().GetName(), WorkerPool: assignment.GetWorkerPool(),
	}
	if latest := latestSubstrateSnapshot(state.GetSnapshots()); latest != nil {
		converted.TemplateUID = latest.GetActorTemplateUid()
		converted.SnapshotScope = substrateSnapshotScope(latest.GetFidelity())
		durable := latest.GetDurableSnapshot()
		if durable.GetState() == ateapipb.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED {
			converted.LastSnapshot = durable.GetObject().GetSnapshotUri()
		} else {
			converted.InProgressSnapshot = latest.GetUuid()
		}
	}
	return converted
}

func substrateWorkerFromProto(worker *ateapipb.Worker) substrateWorker {
	return substrateWorker{
		WorkerName: worker.GetMetadata().GetName(), WorkerPodUID: worker.GetWorkerPodUid(),
		WorkerNamespace: worker.GetWorkerNamespace(), WorkerPool: worker.GetWorkerPool(),
		WorkerPod: worker.GetWorkerPod(), IP: firstNonEmpty(worker.GetStatus().GetIps()),
	}
}

func firstNonEmpty(values []string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
