// Package verification contains the immutable verification record models shared
// by Orka and its storage providers. It does not grant execution authority.
package verification

import (
	"encoding/json"
	"time"
)

const (
	ValidateReport       Action          = "validate-report"
	VerifyPatch          Action          = "verify-patch"
	Reproduced           Conclusion      = "Reproduced"
	NotReproduced        Conclusion      = "Not reproduced under these conditions"
	UnableToValidate     Conclusion      = "Unable to validate"
	Verified             Conclusion      = "Verified for these checks"
	NotFixed             Conclusion      = "Not fixed"
	PartiallyFixed       Conclusion      = "Partially fixed"
	Regression           Conclusion      = "Introduces a regression"
	UnableToVerify       Conclusion      = "Unable to verify"
	SubmissionPreparing  SubmissionState = "preparing"
	SubmissionRunning    SubmissionState = "running"
	SubmissionTerminal   SubmissionState = "terminal"
	SubmissionCancelling SubmissionState = "cancelling"
	DispatchPlanned      DispatchState   = "planned"
	DispatchCreated      DispatchState   = "created"
	DispatchObserved     DispatchState   = "observed"
	DispatchRecorded     DispatchState   = "recorded"
	DispatchFailed       DispatchState   = "failed"
	RunRunning           RunState        = "running"
	RunFinalized         RunState        = "finalized"
	RunCancelled         RunState        = "cancelled"
	RunInterrupted       RunState        = "interrupted"
	RunInvalid           RunState        = "invalid"
)

type Action string

type ApplicationAPIIdentity struct {
	APIVersion      string               `json:"apiVersion"`
	Resource        string               `json:"resource"`
	Name            string               `json:"name"`
	Pointer         string               `json:"pointer,omitempty"`
	UID             string               `json:"uid,omitempty"`
	ResourceVersion string               `json:"resourceVersion,omitempty"`
	Result          ApplicationAPIResult `json:"result"`
}

// API values are reduced to canonical digest/size metadata before transport.
// In particular, a subject cannot exfiltrate credentials through a status field.
type ApplicationAPIResult struct {
	Exists      bool   `json:"exists"`
	Found       bool   `json:"found"`
	ValueDigest string `json:"valueDigest,omitempty"`
	ValueBytes  int    `json:"valueBytes,omitempty"`
}

// Actions use fixed POST-create or exact UID/resourceVersion PUT-update, never
// an upsert that could adopt an unrelated object. Credentials are Pod-bound
// actor tokens in protected target Secrets, not observer-token fallbacks.
type ApplicationAction struct {
	ActorID        string          `json:"actorID"`
	Verb           string          `json:"verb"`
	Resource       json.RawMessage `json:"resource"`
	ExpectedStatus int             `json:"expectedStatus"`
}

type ApplicationActionObservation struct {
	Index               int                      `json:"index"`
	Actor               ApplicationActorIdentity `json:"actor"`
	APIVersion          string                   `json:"apiVersion"`
	Resource            string                   `json:"resource"`
	Name                string                   `json:"name"`
	StatusCode          int                      `json:"statusCode"`
	UID                 string                   `json:"uid,omitempty"`
	ResourceVersion     string                   `json:"resourceVersion,omitempty"`
	Target              ApplicationActionTarget  `json:"target"`
	AuthorizationDenied bool                     `json:"authorizationDenied,omitempty"`
	InitialAPI          *ApplicationAPIIdentity  `json:"initialAPI,omitempty"`
}

type ApplicationActionTarget struct {
	Index           int    `json:"index"`
	APIVersion      string `json:"apiVersion"`
	Resource        string `json:"resource"`
	Name            string `json:"name"`
	MustBeAbsent    bool   `json:"mustBeAbsent"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	OwnerUID        string `json:"ownerUID"`
}

type ApplicationActorIdentity struct {
	ID                 string `json:"id"`
	Namespace          string `json:"namespace"`
	NamespaceUID       string `json:"namespaceUID"`
	ServiceAccountName string `json:"serviceAccountName"`
	ServiceAccountUID  string `json:"serviceAccountUID"`
}

type ApplicationActorPolicy struct {
	ID          string                  `json:"id"`
	Permissions []ApplicationPermission `json:"permissions"`
}

type ApplicationArchiveMetadata struct {
	ArchiveDigest string `json:"archiveDigest"`
	ArchiveBytes  int64  `json:"archiveBytes"`
	ContentBytes  int64  `json:"contentBytes"`
	Entries       int    `json:"entries"`
}

type ApplicationArtifact struct {
	Version        int                          `json:"version"`
	RunID          string                       `json:"runID"`
	AttemptID      string                       `json:"attemptID"`
	TaskID         string                       `json:"taskID"`
	ManifestDigest string                       `json:"manifestDigest"`
	PlanDigest     string                       `json:"planDigest"`
	Side           string                       `json:"side"`
	Source         SourceIdentity               `json:"source"`
	BinaryDigest   string                       `json:"binaryDigest"`
	BinaryBytes    int64                        `json:"binaryBytes"`
	GoVersion      string                       `json:"goVersion"`
	Platform       string                       `json:"platform"`
	Build          ApplicationContainerIdentity `json:"build"`
}

// Environment.Image/ImageID identify the pinned trusted worker/toolchain image.
// No executable build instructions, flags, environment or Dockerfile are input.
type ApplicationBuild struct {
	Profile   string `json:"profile"`
	Package   string `json:"package"`
	GoVersion string `json:"goVersion"`
}

// SchemaDigest covers the complete CRD spec, including served versions, schemas,
// scope, names, subresources and conversion; a single version's schema is not
// sufficient to freeze authority.
type ApplicationCRDIdentity struct {
	Name         string `json:"name"`
	UID          string `json:"uid"`
	SchemaDigest string `json:"schemaDigest"`
}

type ApplicationCheck struct {
	Version       int                         `json:"version"`
	SettleSeconds int                         `json:"settleSeconds,omitempty"`
	Resources     []json.RawMessage           `json:"resources,omitempty"`
	Kubernetes    *ApplicationKubernetesCheck `json:"kubernetes,omitempty"`
	TCP           *ApplicationTCPCheck        `json:"tcp,omitempty"`
	Actions       []ApplicationAction         `json:"actions,omitempty"`
}

// Container identities come from a live API read by the trusted backend or
// observer, not from an application log or an untrusted source fixture.
type ApplicationContainerIdentity struct {
	PodName            string `json:"podName"`
	PodUID             string `json:"podUID"`
	ContainerName      string `json:"containerName"`
	ContainerID        string `json:"containerID"`
	ImageID            string `json:"imageID"`
	JobUID             string `json:"jobUID,omitempty"`
	ServiceAccountName string `json:"serviceAccountName"`
	ServiceAccountUID  string `json:"serviceAccountUID"`
	Namespace          string `json:"namespace,omitempty"`
	NamespaceUID       string `json:"namespaceUID,omitempty"`
}

type ApplicationEndpoint struct {
	ID      string                       `json:"id"`
	Address string                       `json:"address"`
	Port    int                          `json:"port"`
	Pod     ApplicationContainerIdentity `json:"pod"`
}

// Fixtures are trusted TCP marker servers, not arbitrary image entrypoints.
// The backend generates a fresh protected marker; it is never part of a plan.
type ApplicationFixture struct {
	ID      string            `json:"id"`
	Port    int               `json:"port"`
	Kind    string            `json:"kind,omitempty"`
	Profile string            `json:"profile,omitempty"`
	Config  map[string]string `json:"config,omitempty"`
}

type ApplicationFixtureProfile struct {
	ID            string                           `json:"id"`
	Image         string                           `json:"image"`
	ImageID       string                           `json:"imageID"`
	Args          []string                         `json:"args,omitempty"`
	Ports         []int                            `json:"ports"`
	ConfigKeys    []string                         `json:"configKeys,omitempty"`
	ReplicaTarget *ApplicationReplicaTargetProfile `json:"replicaTarget,omitempty"`
	SecretFiles   []ApplicationFixtureSecretFile   `json:"secretFiles,omitempty"`
}

// SecretFiles selects generated synthetic keys only, never arbitrary Secrets.
// Each selected key is mounted at the fixed /fixture-secrets/<ID> path.
type ApplicationFixtureSecretFile struct {
	ID         string `json:"id"`
	SecretName string `json:"secretName"`
	Key        string `json:"key"`
}

// Kubernetes observations are exact named GETs in the run namespace, never
// arbitrary URLs, list queries, log streams, exec or fixture-written verdicts.
type ApplicationKubernetesCheck struct {
	APIVersion  string          `json:"apiVersion"`
	Resource    string          `json:"resource"`
	Name        string          `json:"name"`
	Pointer     string          `json:"pointer"`
	NormalValue json.RawMessage `json:"normalValue,omitempty"`
}

type ApplicationObservation struct {
	Version     int                             `json:"version"`
	Completed   bool                            `json:"completed"`
	PlanDigest  string                          `json:"planDigest"`
	Target      ApplicationTarget               `json:"target"`
	Artifact    ApplicationArtifact             `json:"artifact"`
	Subject     ApplicationContainerIdentity    `json:"subject"`
	Kubernetes  *ApplicationAPIIdentity         `json:"kubernetes,omitempty"`
	APIBaseline *ApplicationAPIIdentity         `json:"apiBaseline,omitempty"`
	TCP         *ApplicationTCPIdentity         `json:"tcp,omitempty"`
	Actions     []ApplicationActionObservation  `json:"actions,omitempty"`
	SubjectTLS  []ApplicationSubjectTLSIdentity `json:"subjectTLS,omitempty"`
	Replica     *ApplicationReplicaObservation  `json:"replica,omitempty"`
}

// Permissions can only become a namespace Role. Wildcards, RBAC mutations,
// credential reads, subresource exec and cluster-scoped access are unsupported.
type ApplicationPermission struct {
	APIGroup      string   `json:"apiGroup"`
	Resources     []string `json:"resources"`
	Verbs         []string `json:"verbs"`
	ResourceNames []string `json:"resourceNames,omitempty"`
}

// ApplicationPlan is run data, not permission to build or deploy. The backend
// must also admit it against its operator-owned image, RBAC and target bounds.
// The complete plan participates in the existing ManifestDigest.
type ApplicationPlan struct {
	Version          int                          `json:"version"`
	Build            ApplicationBuild             `json:"build"`
	Runtime          ApplicationRuntime           `json:"runtime"`
	Resources        []json.RawMessage            `json:"resources,omitempty"`
	Permissions      []ApplicationPermission      `json:"permissions,omitempty"`
	Fixtures         []ApplicationFixture         `json:"fixtures,omitempty"`
	SyntheticSecrets []ApplicationSyntheticSecret `json:"syntheticSecrets,omitempty"`
}

// ApplicationPolicy is operator-owned target-profile data. It is frozen outside
// the model's ApplicationPlan; a backend must compare it to its admitted profile
// and revalidate every CRD identity against the dedicated target API.
type ApplicationPolicy struct {
	Version             int                           `json:"version"`
	Resources           []ApplicationResourceRule     `json:"resources"`
	Actors              []ApplicationActorPolicy      `json:"actors,omitempty"`
	SubjectPermissions  []ApplicationPermission       `json:"subjectPermissions,omitempty"`
	Fixtures            []ApplicationFixtureProfile   `json:"fixtures,omitempty"`
	MaxSyntheticSecrets int                           `json:"maxSyntheticSecrets,omitempty"`
	SubjectEnvironment  map[string]string             `json:"subjectEnvironment,omitempty"`
	SubjectTLS          []ApplicationSubjectTLSBundle `json:"subjectTLS,omitempty"`
}

type ApplicationProtectedMarkerRef struct {
	Namespace    string `json:"namespace"`
	NamespaceUID string `json:"namespaceUID"`
	Name         string `json:"name"`
	UID          string `json:"uid"`
	Key          string `json:"key"`
	Digest       string `json:"digest"`
}

// ApplicationReceiverRoute is published once by the backend, after listener
// readiness and before observation. Its resource version fences all subsequent
// slice changes; it is a locator, not evidence that the data plane works.
type ApplicationReceiverRoute struct {
	Version                      int    `json:"version"`
	Namespace                    string `json:"namespace"`
	ServiceUID                   string `json:"serviceUID"`
	PodUID                       string `json:"podUID"`
	EndpointSliceName            string `json:"endpointSliceName"`
	EndpointSliceUID             string `json:"endpointSliceUID"`
	EndpointSliceResourceVersion string `json:"endpointSliceResourceVersion"`
}

type ApplicationReplicaObservation struct {
	DeploymentUID   string                         `json:"deploymentUID"`
	ReplicaSetUIDs  []string                       `json:"replicaSetUIDs"`
	Pods            []ApplicationContainerIdentity `json:"pods"`
	DesiredReplicas int32                          `json:"desiredReplicas"`
	ReadyReplicas   int32                          `json:"readyReplicas"`
	InitialReplicas int32                          `json:"initialReplicas"`
	MaxReplicas     int32                          `json:"maxReplicas"`
}

// ReplicaTarget fixes one small Deployment target without accepting a PodSpec.
// The backend owns its template, admission, quota and UID descendant lifecycle.
type ApplicationReplicaTargetProfile struct {
	Name            string `json:"name"`
	InitialReplicas int32  `json:"initialReplicas"`
	MaxReplicas     int32  `json:"maxReplicas"`
}

type ApplicationResourceRule struct {
	APIVersion string                  `json:"apiVersion"`
	Kind       string                  `json:"kind"`
	Resource   string                  `json:"resource"`
	Namespaced bool                    `json:"namespaced"`
	Verbs      []string                `json:"verbs"`
	Status     bool                    `json:"status,omitempty"`
	Scale      bool                    `json:"scale,omitempty"`
	CRD        *ApplicationCRDIdentity `json:"crd,omitempty"`
}

type ApplicationRuntime struct {
	Image   string   `json:"image"`
	ImageID string   `json:"imageID"`
	Args    []string `json:"args,omitempty"`
	Ports   []int    `json:"ports,omitempty"`
}

type ApplicationSourceMetadata struct {
	Version  int                         `json:"version"`
	Original ApplicationArchiveMetadata  `json:"original"`
	Patched  *ApplicationArchiveMetadata `json:"patched,omitempty"`
}

// SubjectTLS is synthetic lab material generated by the trusted backend. The
// profile carries names only; CA/private-key bytes never enter plans or reports.
type ApplicationSubjectTLSBundle struct {
	ID       string   `json:"id"`
	DNSNames []string `json:"dnsNames"`
}

type ApplicationSubjectTLSIdentity struct {
	ID           string `json:"id"`
	Namespace    string `json:"namespace"`
	NamespaceUID string `json:"namespaceUID"`
	Name         string `json:"name"`
	UID          string `json:"uid"`
	Digest       string `json:"digest"`
}

// Values are freshly generated by the backend, never supplied to the model.
// MarkerID names the protected copy used by the independent receive observer.
type ApplicationSyntheticSecret struct {
	Name string                          `json:"name"`
	Keys []ApplicationSyntheticSecretKey `json:"keys"`
}

type ApplicationSyntheticSecretKey struct {
	Name     string `json:"name"`
	MarkerID string `json:"markerID"`
}

type ApplicationTCPCheck struct {
	Target        string `json:"target"`
	Port          int    `json:"port"`
	Request       []byte `json:"request,omitempty"`
	MarkerID      string `json:"markerID"`
	MaxBytes      int    `json:"maxBytes"`
	Mode          string `json:"mode,omitempty"`
	WindowSeconds int    `json:"windowSeconds,omitempty"`
	Terminator    []byte `json:"terminator,omitempty"`
	Response      []byte `json:"response,omitempty"`
}

type ApplicationTCPIdentity struct {
	Endpoint      ApplicationEndpoint            `json:"endpoint"`
	MarkerID      string                         `json:"markerID"`
	MarkerDigest  string                         `json:"markerDigest"`
	MarkerBytes   int                            `json:"markerBytes"`
	Bytes         int                            `json:"bytes"`
	Result        ApplicationTCPValue            `json:"result"`
	Mode          string                         `json:"mode,omitempty"`
	WindowSeconds int                            `json:"windowSeconds,omitempty"`
	Connections   int                            `json:"connections,omitempty"`
	MarkerRef     *ApplicationProtectedMarkerRef `json:"markerRef,omitempty"`
	ReceiverRoute *ApplicationReceiverRoute      `json:"receiverRoute,omitempty"`
}

type ApplicationTCPValue struct {
	Connected     bool `json:"connected"`
	MarkerMatched bool `json:"markerMatched"`
}

type ApplicationTarget struct {
	ClusterUID   string `json:"clusterUID"`
	Namespace    string `json:"namespace"`
	NamespaceUID string `json:"namespaceUID"`
}

type Assessment struct {
	Conclusion Conclusion    `json:"conclusion"`
	Reason     string        `json:"reason"`
	Checks     []CheckResult `json:"checks"`
}

type Binding struct {
	RunID          string `json:"runID"`
	AttemptID      string `json:"attemptID"`
	OriginalTaskID string `json:"originalTaskID"`
	PatchedTaskID  string `json:"patchedTaskID"`
	ManifestDigest string `json:"manifestDigest"`
}

type BlobReference struct {
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
}

type CapturedOutput struct {
	Digest    string `json:"digest"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

type Check struct {
	ID             string            `json:"id"`
	Kind           string            `json:"kind"`
	Command        []string          `json:"command"`
	HTTP           *HTTPCheck        `json:"http,omitempty"`
	Application    *ApplicationCheck `json:"application,omitempty"`
	Stdin          string            `json:"stdin,omitempty"`
	Healthy        Expectation       `json:"healthy"`
	Failure        Expectation       `json:"failure"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	Lifecycle      []string          `json:"lifecycle,omitempty"`
}

type CheckResult struct {
	CheckID         string `json:"checkID"`
	Outcome         string `json:"outcome"`
	Reason          string `json:"reason,omitempty"`
	OriginalOutcome string `json:"originalOutcome,omitempty"`
	PatchedOutcome  string `json:"patchedOutcome,omitempty"`
}

type Conclusion string

type DeclaredChange struct {
	Kind        string   `json:"kind"`
	Paths       []string `json:"paths,omitempty"`
	Description string   `json:"description"`
}

type DispatchState string

type EarlierValidationReference struct {
	RunID          string `json:"runID"`
	ReportDigest   string `json:"reportDigest"`
	ManifestDigest string `json:"manifestDigest"`
	SealDigest     string `json:"sealDigest"`
}

type Environment struct {
	Image        string                   `json:"image"`
	ImageID      string                   `json:"imageID"`
	Platform     string                   `json:"platform"`
	Profile      string                   `json:"profile"`
	Variables    map[string]string        `json:"variables,omitempty"`
	Dependencies map[string]string        `json:"dependencies,omitempty"`
	Services     []Service                `json:"services,omitempty"`
	Requirements []EnvironmentRequirement `json:"requirements,omitempty"`
}

type EnvironmentRequirement struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type EvidenceRecord struct {
	Observation Observation `json:"observation"`
	Digest      string      `json:"digest"`
	Rejection   string      `json:"rejection,omitempty"`
}

type ExecutionEvidence struct {
	Observation Observation       `json:"observation"`
	Blobs       map[string][]byte `json:"blobs"`
}

type Expectation struct {
	ExitCode int               `json:"exitCode"`
	Stdout   string            `json:"stdout"`
	Services map[string]string `json:"services,omitempty"`
}

type FrozenFile struct {
	Path       string `json:"path"`
	Content    []byte `json:"content"`
	Digest     string `json:"digest"`
	Executable bool   `json:"executable,omitempty"`
}

// HTTPCheck declares external behavior, not execution of a subject function.
// The server is untrusted; only the supervisor performs and records the GET.
type HTTPCheck struct {
	Version       int      `json:"version"`
	ServerCommand []string `json:"serverCommand"`
	Path          string   `json:"path"`
}

type Incident struct {
	Kind   string `json:"kind"`
	Slot   string `json:"slot"`
	Digest string `json:"digest"`
	Reason string `json:"reason"`
}

type KubernetesDispatch struct {
	RunID       string        `json:"runID"`
	Side        string        `json:"side"`
	CheckID     string        `json:"checkID"`
	JobName     string        `json:"jobName"`
	SpecDigest  string        `json:"specDigest"`
	JobUID      string        `json:"jobUID,omitempty"`
	PodUID      string        `json:"podUID,omitempty"`
	ContainerID string        `json:"containerID,omitempty"`
	State       DispatchState `json:"state"`
	Failure     string        `json:"failure,omitempty"`
}

type KubernetesSubmission struct {
	Namespace        string            `json:"namespace"`
	RequestID        string            `json:"requestID"`
	SubmittedBy      string            `json:"submittedBy"`
	AttemptID        string            `json:"attemptID"`
	OriginalTaskName string            `json:"originalTaskName"`
	PatchedTaskName  string            `json:"patchedTaskName,omitempty"`
	OriginalTaskUID  string            `json:"originalTaskUID,omitempty"`
	PatchedTaskUID   string            `json:"patchedTaskUID,omitempty"`
	Manifest         Manifest          `json:"manifest"`
	Provenance       map[string][]byte `json:"-"`
	Binding          *Binding          `json:"binding,omitempty"`
	RunID            string            `json:"runID,omitempty"`
	State            SubmissionState   `json:"state"`
	Failure          string            `json:"failure,omitempty"`
	CreatedAt        time.Time         `json:"createdAt"`
	UpdatedAt        time.Time         `json:"updatedAt"`
	Assessment       *Assessment       `json:"assessment,omitempty"`
	RequiredChecks   int               `json:"requiredChecks"`
	RecordedChecks   int               `json:"recordedChecks"`
}

type Manifest struct {
	Version                   int                         `json:"version"`
	Action                    Action                      `json:"action,omitempty"`
	ReportDigest              string                      `json:"reportDigest,omitempty"`
	EarlierValidation         *EarlierValidationReference `json:"earlierValidation,omitempty"`
	DeclaredChanges           []DeclaredChange            `json:"declaredChanges,omitempty"`
	Problem                   string                      `json:"problem"`
	Scope                     []string                    `json:"scope"`
	Gaps                      []string                    `json:"gaps"`
	Sources                   Sources                     `json:"sources"`
	Environment               Environment                 `json:"environment"`
	Files                     []FrozenFile                `json:"files"`
	Checks                    []Check                     `json:"checks"`
	Application               *ApplicationPlan            `json:"application,omitempty"`
	ApplicationPolicy         *ApplicationPolicy          `json:"applicationPolicy,omitempty"`
	ApplicationSourceMetadata *ApplicationSourceMetadata  `json:"applicationSourceMetadata,omitempty"`
}

type Observation struct {
	RunID           string                    `json:"runID"`
	AttemptID       string                    `json:"attemptID"`
	TaskID          string                    `json:"taskID"`
	ManifestDigest  string                    `json:"manifestDigest"`
	Side            string                    `json:"side"`
	CheckID         string                    `json:"checkID"`
	SourceTree      string                    `json:"sourceTree"`
	ImageID         string                    `json:"imageID"`
	ContainerID     string                    `json:"containerID"`
	JobUID          string                    `json:"jobUID,omitempty"`
	PodUID          string                    `json:"podUID,omitempty"`
	Origin          string                    `json:"origin"`
	StartedAt       time.Time                 `json:"startedAt"`
	FinishedAt      time.Time                 `json:"finishedAt"`
	Executed        bool                      `json:"executed"`
	HTTPCompleted   bool                      `json:"httpCompleted,omitempty"`
	ExitCode        *int                      `json:"exitCode,omitempty"`
	StdoutDigest    string                    `json:"stdoutDigest,omitempty"`
	StdoutBytes     int                       `json:"stdoutBytes"`
	StderrDigest    string                    `json:"stderrDigest,omitempty"`
	StderrBytes     int                       `json:"stderrBytes"`
	TimedOut        bool                      `json:"timedOut"`
	Skipped         bool                      `json:"skipped"`
	OutputTruncated bool                      `json:"outputTruncated"`
	SetupError      string                    `json:"setupError,omitempty"`
	ServiceOutputs  map[string]CapturedOutput `json:"serviceOutputs,omitempty"`
	Application     *ApplicationObservation   `json:"application,omitempty"`
}

type Record struct {
	Manifest   Manifest         `json:"manifest"`
	Binding    Binding          `json:"binding"`
	Provenance []BlobReference  `json:"provenance"`
	Evidence   []EvidenceRecord `json:"evidence"`
	State      RunState         `json:"state"`
	Assessment Assessment       `json:"assessment"`
	Seal       *Seal            `json:"seal,omitempty"`
	Incidents  []Incident       `json:"incidents"`
}

type RunState string

type Seal struct {
	Digest     string     `json:"digest"`
	Content    []byte     `json:"content"`
	Assessment Assessment `json:"assessment"`
}

type Service struct {
	ID          string   `json:"id"`
	Command     []string `json:"command"`
	Port        int      `json:"port"`
	ReadyOutput string   `json:"readyOutput"`
}

type SourceIdentity struct {
	Commit        string `json:"commit,omitempty"`
	Tree          string `json:"tree"`
	ArchiveDigest string `json:"archiveDigest"`
}

type Sources struct {
	Repository  string         `json:"repository"`
	Original    SourceIdentity `json:"original"`
	Patched     SourceIdentity `json:"patched"`
	Paths       []string       `json:"paths,omitempty"`
	PatchDigest string         `json:"patchDigest,omitempty"`
	DiffDigest  string         `json:"diffDigest"`
}

type SubmissionState string
