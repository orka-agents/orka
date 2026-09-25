package workspace

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
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

type grpcSubstrateSessionIdentityClient struct {
	conn     *grpc.ClientConn
	client   ateapipb.ActorIdentityClient
	atespace string
}

func newGRPCSubstrateControlClient(cfg SubstrateConfig) (*grpcSubstrateControlClient, error) {
	conn, err := newSubstrateConnection(cfg, true)
	if err != nil {
		return nil, err
	}
	return &grpcSubstrateControlClient{conn: conn, client: ateapipb.NewControlClient(conn), atespace: strings.TrimSpace(cfg.Atespace)}, nil
}

func newGRPCSubstrateSessionIdentityClient(cfg SubstrateConfig) (*grpcSubstrateSessionIdentityClient, error) {
	// ActorIdentity authenticates the worker Pod hosting this exact Actor.
	// Never append the controller's control-plane identity to that request.
	cfg.APICertFile, cfg.APIKeyFile, cfg.APIBearerTokenFile = "", "", ""
	conn, err := newSubstrateConnection(cfg, false)
	if err != nil {
		return nil, err
	}
	return &grpcSubstrateSessionIdentityClient{conn: conn, client: ateapipb.NewActorIdentityClient(conn), atespace: cfg.Atespace}, nil
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
		ateapipb.Control_PauseActor_FullMethodName,
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

func (c *grpcSubstrateSessionIdentityClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func substrateTransportCredentials(cfg SubstrateConfig) (credentials.TransportCredentials, error) {
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
	return credentials.NewTLS(tlsConfig), nil
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
	return substrateActorFromProto(actor), nil
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

func (c *grpcSubstrateControlClient) ResumeActor(ctx context.Context, actorID string, boot bool) (*substrateActor, error) {
	ref, err := substrateObjectRef(actorID, c.atespace)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: ref, Boot: boot})
	if err != nil {
		return nil, substrateControlError("resume actor", err)
	}
	return substrateActorFromProto(resp.GetActor()), nil
}

func (c *grpcSubstrateControlClient) SuspendActor(ctx context.Context, actorID string) (*substrateActor, error) {
	ref, err := substrateObjectRef(actorID, c.atespace)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: ref})
	if err != nil {
		return nil, substrateControlError("suspend actor", err)
	}
	return substrateActorFromProto(resp.GetActor()), nil
}

func (c *grpcSubstrateControlClient) DeleteActor(ctx context.Context, actorID string) error {
	ref, err := substrateObjectRef(actorID, c.atespace)
	if err != nil {
		return err
	}
	// Native AnyState terminates without taking a memory snapshot. Cleanup
	// callers prove their own lifetime/ownership fences before invoking this.
	_, err = c.client.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: ref, AnyState: true})
	return substrateControlError("delete actor", err)
}

func (c *grpcSubstrateControlClient) ListWorkers(ctx context.Context) ([]substrateWorker, error) {
	var workers []substrateWorker
	pages := substratePages{}
	for {
		resp, err := c.client.ListWorkers(ctx, &ateapipb.ListWorkersRequest{PageSize: 1000, PageToken: pages.token})
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
		resp, err := c.client.ListActors(ctx, &ateapipb.ListActorsRequest{Atespace: c.atespace, PageSize: 1000, PageToken: pages.token})
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

func (c *grpcSubstrateSessionIdentityClient) MintJWT(ctx context.Context, req substrateMintJWTRequest, bearerToken string) (string, error) {
	if strings.TrimSpace(req.Atespace) == "" || strings.TrimSpace(req.ActorName) == "" || strings.TrimSpace(req.ActorUID) == "" {
		return "", NewError("mint actor identity", ErrorKindFailedPrecondition, "ActorIdentity requires the exact Actor Atespace, name and UID", false, nil)
	}
	// Replace, rather than append, so an inherited controller credential can
	// never take precedence over the hosting worker's Pod-bound token.
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set("authorization", "Bearer "+strings.TrimSpace(bearerToken))
	ctx = metadata.NewOutgoingContext(ctx, md)
	resp, err := c.client.MintJWT(ctx, &ateapipb.MintJWTRequest{
		Audience: append([]string(nil), req.Audience...), Atespace: req.Atespace,
		ActorName: req.ActorName, ActorUid: req.ActorUID,
	})
	if err != nil {
		return "", substrateControlError("mint actor identity", err)
	}
	return resp.GetActorJwt(), nil
}

func substrateActorFromProto(actor *ateapipb.Actor) *substrateActor {
	if actor == nil {
		return nil
	}
	meta, state := actor.GetMetadata(), actor.GetStatus()
	assignment := state.GetWorkerAssignment()
	scope := SubstrateSnapshotContentScope("")
	switch state.GetExternalSnapshot().GetContentScope() {
	case ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA:
		scope = SubstrateSnapshotContentScopeData
	case ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL:
		scope = SubstrateSnapshotContentScopeFull
	}
	return &substrateActor{
		ActorID: meta.GetName(), Atespace: meta.GetAtespace(), ActorUID: meta.GetUid(), ActorVersion: meta.GetVersion(),
		TemplateNamespace: actor.GetActorTemplate().GetAtespace(), TemplateName: actor.GetActorTemplate().GetName(),
		TemplateUID:  state.GetCurrentActorTemplateUid(),
		Status:       "STATUS_" + strings.TrimPrefix(state.GetState().String(), "ACTOR_STATE_"),
		PodNamespace: assignment.GetWorkerNamespace(), PodName: assignment.GetWorkerPod(),
		PodUID: assignment.GetWorkerPodUid(), PodIP: assignment.GetWorkerPodIp(),
		WorkerName: assignment.GetWorker().GetName(), WorkerPool: assignment.GetWorkerPool(),
		LastSnapshot: state.GetExternalSnapshot().GetSnapshotUri(), SnapshotScope: scope,
		InProgressSnapshot: state.GetInProgressSnapshotName(),
	}
}

func substrateWorkerFromProto(worker *ateapipb.Worker) substrateWorker {
	return substrateWorker{
		WorkerName: worker.GetMetadata().GetName(), WorkerPodUID: worker.GetWorkerPodUid(),
		WorkerNamespace: worker.GetWorkerNamespace(), WorkerPool: worker.GetWorkerPool(),
		WorkerPod: worker.GetWorkerPod(), IP: worker.GetIp(),
	}
}
