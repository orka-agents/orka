package patchverification

const (
	ApplicationObserverOrigin  = "application-observer"
	ApplicationTCPReceive      = "receive"
	ApplicationSubject         = "subject"
	ApplicationFixtureImage    = "image"
	ApplicationFixtureReplica  = "replica"
	ApplicationFixtureMarker   = "marker"
	ApplicationFixtureReceiver = "receiver"

	ApplicationActionCreate = "create"
	ApplicationActionUpdate = "update"
	ApplicationVerbGet      = "get"
	ApplicationVerbList     = "list"
	ApplicationVerbWatch    = "watch"
	ApplicationVerbPatch    = "patch"
	ApplicationFrameAction  = "action"
	ApplicationFrameFinal   = "final"
	ApplicationFrameFailure = "failure"

	applicationAppsGroup   = "apps"
	applicationAppsVersion = "apps/v1"
	applicationDeployments = "deployments"
	applicationPods        = "pods"
	applicationSecrets     = "secrets"
	applicationGitApply    = "apply"
)
