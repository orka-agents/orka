package applicationexec

import appsv1 "k8s.io/api/apps/v1"

const (
	serviceAccountKind = "ServiceAccount"
	roleKind           = "Role"
	namespaceKind      = "Namespace"
	jobKind            = "Job"
	podKind            = "Pod"
	serviceKind        = "Service"
	deploymentKind     = "Deployment"
	replicaSetKind     = "ReplicaSet"
	appsAPIVersion     = appsv1.GroupName + "/v1"
)

const (
	verbGet    = "get"
	verbList   = "list"
	verbWatch  = "watch"
	verbCreate = "create"
	verbUpdate = "update"
	verbPatch  = "patch"

	resourceConfigMaps      = "configmaps"
	resourceIngresses       = "ingresses"
	resourceEndpoints       = "endpoints"
	resourceServices        = "services"
	resourcePods            = "pods"
	resourceDeployments     = "deployments"
	resourceDeploymentScale = resourceDeployments + "/scale"
)

const (
	roleWorker   = "worker"
	roleSubject  = "subject"
	roleObserver = "observer"
	roleMarker   = "marker"
	roleReplica  = "replica"
	roleFixture  = "fixture"
	rolePositive = "positive"
	rolePrepare  = "prepare"
	roleBuild    = "build"

	markerAccountName   = "orka-marker"
	observerAccountName = "orka-observer"
	replicaAccountName  = "orka-replica"
	subjectAccountName  = "orka-" + roleSubject
)

const (
	workerModePrepare  = "prepare"
	workerModeBuild    = "build"
	workerModeSubject  = "subject"
	workerModeObserve  = "observe"
	workerModeBoundary = "boundary"
	workerModeFixture  = "fixture"
	workerModeMarker   = "marker"
	workerModeInstall  = "install"
)

const (
	fixtureKindImage   = "image"
	fixtureKindMarker  = "marker"
	fixtureKindReplica = "replica"
	receiveMode        = "receive"
	subjectTarget      = "subject"

	runnerVolumeName    = "runner"
	sourceVolumeName    = "source"
	artifactsVolumeName = "artifacts"
	markerVolumeName    = "marker"
	markerSecretName    = "orka-marker"
	legacyMarkerKey     = "value"
	caCertificateKey    = "ca.crt"
	kubeconfigKey       = "kubeconfig"
	namespaceKey        = "namespace"

	podSecurityRestricted = "restricted"
	effectCreateRejected  = "create-rejected"
)
