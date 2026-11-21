export type PageId = "discover" | "tasks" | "agents" | "install";
export type InstallationState = "absent" | "partial" | "installing" | "ready" | "degraded";
export type TaskPhase = "Pending" | "Running" | "Finalizing" | "Succeeded" | "Failed" | "Scheduled" | "Cancelled" | "Unknown";

export interface KubeContext {
  name: string;
  cluster: string;
  server: string;
  namespace: string;
  current: boolean;
}

export interface PermissionCheck {
  resource: string;
  verb: string;
  allowed: boolean;
}

export interface ComponentHealth {
  name: string;
  kind: "Deployment" | "Service" | "PersistentVolumeClaim";
  ready: boolean;
  detail: string;
}

export interface OrkaTask {
  uid: string;
  name: string;
  namespace: string;
  type: "container" | "ai" | "agent" | "unknown";
  phase: TaskPhase;
  agentName?: string;
  createdAt: string;
  startedAt?: string;
  completedAt?: string;
  attempts: number;
}

export interface OrkaAgent {
  uid: string;
  name: string;
  namespace: string;
  ready: boolean;
  runtime: string;
  model: string;
  activeTasks: number;
  lastUsed?: string;
}

export interface ClusterReport {
  context: KubeContext;
  crawledAt: string;
  kubernetesVersion: string;
  nodeCount: number;
  defaultStorageClass?: string;
  orka: {
    state: InstallationState;
    installed: boolean;
    namespace: string;
    releaseName?: string;
    version?: string;
    crdCount: number;
    expectedCrdCount: number;
    controllerReady: boolean;
    message: string;
  };
  counts: {
    tasks: number;
    agents: number;
    providers: number;
    monitors: number;
    runtimePools: number;
  };
  tasks: OrkaTask[];
  agents: OrkaAgent[];
  components: ComponentHealth[];
  permissions: PermissionCheck[];
  warnings: string[];
}

export interface CrawlInput {
  context: string;
  namespace?: string;
}

export interface RunTaskInput {
  idempotencyKey: string;
  context: string;
  namespace: string;
  mode: "smoke" | "agent";
  agentName?: string;
  prompt?: string;
}

export interface InstallPlan {
  id: string;
  context: string;
  cluster: string;
  server: string;
  fingerprint: string;
  namespace: string;
  runtimeNamespace: string;
  releaseName: string;
  chartVersion: string;
  chartDigest: string;
  expiresAt: string;
  allowed: boolean;
  blockers: string[];
  warnings: string[];
}

export interface InstallInput {
  planId: string;
  context: string;
  confirmation: string;
  acknowledgeClusterAccess: boolean;
  acknowledgePersistence: boolean;
}
