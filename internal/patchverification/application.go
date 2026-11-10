package patchverification

import (
	"encoding/json"
	verificationv1 "github.com/orka-agents/orka/pkg/storage/v1/verification"
)

const (
	ApplicationProtocolVersion           = 1
	ApplicationRuntimeProfile            = "application-runtime"
	ApplicationBuildGoPackage            = "go-package-v1"
	ApplicationPolicyVersion             = "application-v1"
	ApplicationPolicyKey                 = "orka.application.policy"
	ApplicationObserverImageKey          = "orka.application.observer-image"
	ApplicationObserverImageIDKey        = "orka.application.observer-image-id"
	ApplicationTargetLabel               = "orka.ai/application-target"
	ApplicationRunLabel                  = "applicationexec.orka.ai/run"
	ApplicationWorkerPath                = "/usr/local/bin/orka-application-worker"
	ApplicationBinaryName                = "application"
	ApplicationArtifactName              = "artifact.json"
	ApplicationInputPath                 = "/input/input.json"
	ApplicationSourcePath                = "/source/source.tar"
	ApplicationSourceVolumePath          = "/source"
	ApplicationSourceDirectory           = "archives"
	ApplicationOriginalSourceName        = "original.tar"
	ApplicationPatchedSourceName         = "patched.tar"
	ApplicationOriginalSourceSubPath     = ApplicationSourceDirectory + "/" + ApplicationOriginalSourceName
	ApplicationPatchedSourceSubPath      = ApplicationSourceDirectory + "/" + ApplicationPatchedSourceName
	ApplicationArtifactsPath             = "/artifacts"
	ApplicationWorkPath                  = "/work"
	ApplicationMarkerPath                = "/marker/value"
	ApplicationKubeconfigPath            = "/target/kubeconfig"
	ApplicationSubjectConfigPath         = "/subject-config"
	ApplicationFixtureSecretsPath        = "/fixture-secrets"
	ApplicationRuntimeQualificationPath  = "/input/runtime.json"
	ApplicationRuntimeQualificationKey   = "orka.application.runtime-qualification"
	ApplicationRuntimeGVisorBoundedV1    = "gvisor-bounded-v1"
	MaxApplicationSourceContentBytes     = 256 << 20
	MaxApplicationSourceBytes            = 320 << 20
	MaxApplicationSourceEntries          = 32768
	MaxApplicationBinaryBytes            = 256 << 20
	MaxApplicationInputBytes             = 1 << 20
	MaxApplicationReportBytes            = 64 << 10
	MaxApplicationResources              = 32
	MaxApplicationTCPBytes               = 16 << 10
	MaxApplicationPreparationReportBytes = 2 << 20
)

type ApplicationPlan = verificationv1.ApplicationPlan

type ApplicationBuild = verificationv1.ApplicationBuild

type ApplicationRuntime = verificationv1.ApplicationRuntime

type ApplicationPermission = verificationv1.ApplicationPermission

type ApplicationFixture = verificationv1.ApplicationFixture

type ApplicationCheck = verificationv1.ApplicationCheck

type ApplicationKubernetesCheck = verificationv1.ApplicationKubernetesCheck

type ApplicationTCPCheck = verificationv1.ApplicationTCPCheck

type ApplicationAPIValue struct {
	Exists bool            `json:"exists"`
	Found  bool            `json:"found"`
	Value  json.RawMessage `json:"value,omitempty"`
}

type ApplicationTCPValue = verificationv1.ApplicationTCPValue

type ApplicationTarget = verificationv1.ApplicationTarget

type ApplicationContainerIdentity = verificationv1.ApplicationContainerIdentity

type ApplicationArtifact = verificationv1.ApplicationArtifact

type ApplicationEndpoint = verificationv1.ApplicationEndpoint

// WorkerInput is mounted read-only by the backend. Build uses SourcePath;
// subject verifies Artifact then execs the read-only binary; observe uses an
// independently scoped target kubeconfig and, for TCP only, a protected marker.
// None of these worker modes accepts a shell command.
type ApplicationWorkerInput struct {
	Version         int                                `json:"version"`
	Manifest        Manifest                           `json:"manifest"`
	Binding         Binding                            `json:"binding"`
	Side            string                             `json:"side"`
	CheckID         string                             `json:"checkID,omitempty"`
	TaskUID         string                             `json:"taskUID"`
	Target          ApplicationTarget                  `json:"target"`
	Worker          ApplicationContainerIdentity       `json:"worker"`
	Artifact        *ApplicationArtifact               `json:"artifact,omitempty"`
	Subject         *ApplicationContainerIdentity      `json:"subject,omitempty"`
	Endpoints       []ApplicationEndpoint              `json:"endpoints,omitempty"`
	MarkerDigest    string                             `json:"markerDigest,omitempty"`
	Actors          []ApplicationActorIdentity         `json:"actors,omitempty"`
	MarkerRef       *ApplicationProtectedMarkerRef     `json:"markerRef,omitempty"`
	FixtureID       string                             `json:"fixtureID,omitempty"`
	ActionTargets   []ApplicationActionTarget          `json:"actionTargets,omitempty"`
	ReceiverService *ApplicationServiceIdentity        `json:"receiverService,omitempty"`
	APIBaseline     *ApplicationAPIIdentity            `json:"apiBaseline,omitempty"`
	SubjectTLS      []ApplicationSubjectTLSIdentity    `json:"subjectTLS,omitempty"`
	FixtureSecrets  []ApplicationFixtureSecretIdentity `json:"fixtureSecrets,omitempty"`
}

type ApplicationBuildReport struct {
	Version  int                 `json:"version"`
	TaskUID  string              `json:"taskUID"`
	PodUID   string              `json:"podUID"`
	Artifact ApplicationArtifact `json:"artifact"`
}

type ApplicationReport struct {
	Version  int               `json:"version"`
	TaskUID  string            `json:"taskUID"`
	PodUID   string            `json:"podUID"`
	Evidence ExecutionEvidence `json:"evidence"`
}

type ApplicationObservation = verificationv1.ApplicationObservation

type ApplicationReplicaObservation = verificationv1.ApplicationReplicaObservation

type ApplicationAPIIdentity = verificationv1.ApplicationAPIIdentity

type ApplicationTCPIdentity = verificationv1.ApplicationTCPIdentity

type ApplicationAPIResult = verificationv1.ApplicationAPIResult

type ApplicationBoundaryInput struct {
	Version              int                              `json:"version"`
	ID                   string                           `json:"id"`
	TaskUID              string                           `json:"taskUID"`
	Target               ApplicationTarget                `json:"target"`
	CanaryHost           string                           `json:"canaryHost"`
	CanaryPort           int                              `json:"canaryPort"`
	PositiveControl      bool                             `json:"positiveControl"`
	APIAddress           string                           `json:"apiAddress,omitempty"`
	ServiceAccountName   string                           `json:"serviceAccountName,omitempty"`
	SourceHost           string                           `json:"sourceHost,omitempty"`
	RuntimeQualification *ApplicationRuntimeQualification `json:"runtimeQualification,omitempty"`
	RuntimeLimits        *ApplicationRuntimeLimits        `json:"runtimeLimits,omitempty"`
	FileSizeRole         string                           `json:"fileSizeRole,omitempty"`
}

// Boundary reports contain measured controls, not an asserted "sandbox safe".
// The backend must bind the report to the exact admitted probe Pod and compare
// a reachable positive canary control with the denied subject-profile probe.
type ApplicationBoundaryReport struct {
	Version                    int               `json:"version"`
	InputDigest                string            `json:"inputDigest"`
	TaskUID                    string            `json:"taskUID"`
	PodUID                     string            `json:"podUID"`
	UID                        int               `json:"uid"`
	GID                        int               `json:"gid"`
	PID                        int               `json:"pid"`
	Capabilities               map[string]string `json:"capabilities"`
	NoNewPrivileges            bool              `json:"noNewPrivileges"`
	SeccompMode                int               `json:"seccompMode"`
	ReadOnlyRoot               bool              `json:"readOnlyRoot"`
	MemoryMax                  uint64            `json:"memoryMax"`
	PIDsMax                    uint64            `json:"pidsMax"`
	SwapMax                    uint64            `json:"swapMax"`
	CPUQuota                   uint64            `json:"cpuQuota"`
	CPUPeriod                  uint64            `json:"cpuPeriod"`
	CanaryReachable            bool              `json:"canaryReachable"`
	APIStatuses                map[string]int    `json:"apiStatuses,omitempty"`
	APIUserUID                 string            `json:"apiUserUID,omitempty"`
	SourceHTTPSVerified        bool              `json:"sourceHTTPSVerified,omitempty"`
	SourceHTTPStatus           int               `json:"sourceHTTPStatus,omitempty"`
	RuntimeProfile             string            `json:"runtimeProfile,omitempty"`
	RuntimeQualificationDigest string            `json:"runtimeQualificationDigest,omitempty"`
	PIDLimitSource             string            `json:"pidLimitSource,omitempty"`
	SwapLimitSource            string            `json:"swapLimitSource,omitempty"`
	GuestPIDSoft               uint64            `json:"guestPIDSoft,omitempty"`
	GuestPIDHard               uint64            `json:"guestPIDHard,omitempty"`
	FileSizeSoft               uint64            `json:"fileSizeSoft,omitempty"`
	FileSizeHard               uint64            `json:"fileSizeHard,omitempty"`
}
