package controller

// The generic workspace coordinator owns generic API objects only. Provider-native
// resources remain adapter-owned and intentionally do not appear in Orka core RBAC.
//
// +kubebuilder:rbac:groups=workspace.orka.ai,resources=executionworkspaceproviders,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=workspace.orka.ai,resources=executionworkspaceproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workspace.orka.ai,resources=executionworkspaceproviders/finalizers,verbs=update
// +kubebuilder:rbac:groups=workspace.orka.ai,resources=executionworkspaceclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=workspace.orka.ai,resources=executionworkspaceclasses,verbs=use
// +kubebuilder:rbac:groups=workspace.orka.ai,resources=executionworkspaceclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workspace.orka.ai,resources=executionworkspacepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=workspace.orka.ai,resources=executionworkspacepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workspace.orka.ai,resources=executionworkspaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=workspace.orka.ai,resources=executionworkspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workspace.orka.ai,resources=executionworkspaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
