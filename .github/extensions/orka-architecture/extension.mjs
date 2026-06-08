import { createServer } from "node:http";
import { randomUUID } from "node:crypto";
import { writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";
import { CanvasError, createCanvas, joinSession } from "@github/copilot-sdk/extension";

const layers = [
    {
        id: "entry",
        title: "Entry points",
        description: "Humans, automation, and compatible AI clients enter through the controller.",
    },
    {
        id: "controller",
        title: "Orka controller",
        description: "The central deployment hosts APIs, reconcilers, auth, queues, and durable state.",
    },
    {
        id: "kubernetes",
        title: "Kubernetes control plane",
        description: "Declarative resources become Jobs and hardened Pods with native scheduling.",
    },
    {
        id: "workers",
        title: "Worker fleet",
        description: "Each task runs in an isolated worker Pod selected by task type and agent runtime.",
    },
    {
        id: "integrations",
        title: "External integrations",
        description: "Providers, custom tools, GitHub workflows, and notifications extend execution.",
    },
];

const components = [
    {
        id: "web-dashboard",
        title: "Web Dashboard",
        subtitle: "Embedded React UI for chat, tasks, scans, logs, and artifacts",
        layer: "entry",
        tier: "optional",
        category: "Experience",
        accent: "#8957e5",
        description: "A Vite and TanStack Router dashboard compiled into the controller binary so operators do not need a separate frontend deployment.",
        responsibilities: ["Interactive chat", "Task and agent views", "Security scan dashboards", "Log and artifact browsing"],
    },
    {
        id: "api-clients",
        title: "REST and AI Clients",
        subtitle: "CLI, CI/CD, Cursor, Continue, Claude-native, and OpenAI-compatible clients",
        layer: "entry",
        tier: "core",
        category: "Access",
        accent: "#2f81f7",
        description: "Clients call Orka through REST endpoints plus OpenAI and Anthropic compatibility routes.",
        responsibilities: ["Task CRUD", "Chat streaming", "OpenAI chat completions", "Anthropic messages"],
    },
    {
        id: "gitops-crds",
        title: "GitOps and CRDs",
        subtitle: "Task, Agent, Tool, Provider, Skill, and RepositoryScan manifests",
        layer: "entry",
        tier: "core",
        category: "Declarative",
        accent: "#1f883d",
        description: "Teams can drive Orka declaratively with Kubernetes resources and GitOps workflows.",
        responsibilities: ["Task declarations", "Agent configuration", "Tool and provider registration", "RepositoryScan schedules"],
    },
    {
        id: "api-chat",
        title: "API Server and Chat",
        subtitle: "Fiber REST API, SSE streaming, chat orchestration, and compatibility endpoints",
        layer: "controller",
        tier: "core",
        category: "Control",
        accent: "#2f81f7",
        description: "The public and internal HTTP surface routes requests, streams chat updates, serves the UI, and receives worker callbacks.",
        responsibilities: ["REST API", "OpenAI-compatible API", "Anthropic-compatible API", "Internal worker API"],
    },
    {
        id: "auth-middleware",
        title: "Auth Middleware",
        subtitle: "ServiceAccount tokens plus optional OIDC and Kontxt TxToken validation",
        layer: "controller",
        tier: "core",
        category: "Security",
        accent: "#d1242f",
        description: "Authentication and namespace authorization are enforced before API or worker traffic reaches handlers.",
        responsibilities: ["TokenReview auth", "Optional OIDC validation", "Optional Kontxt TxToken constraints", "Namespace isolation"],
    },
    {
        id: "reconcilers",
        title: "Reconcilers",
        subtitle: "Task, Agent, Tool, Provider, Skill, and RepositoryScan controllers",
        layer: "controller",
        tier: "core",
        category: "Control",
        accent: "#1f883d",
        description: "Controller-runtime reconcilers watch Orka resources and drive lifecycle changes through Jobs, status updates, and policy enforcement.",
        responsibilities: ["Task lifecycle", "Agent and provider references", "RepositoryScan orchestration", "Owner references and status"],
    },
    {
        id: "priority-queue",
        title: "Priority Queue",
        subtitle: "Priority scheduling, retry policy, backoff, and session locking",
        layer: "controller",
        tier: "core",
        category: "Scheduling",
        accent: "#fb8500",
        description: "Pending work is ordered by priority and coordinated with per-session locks so related tasks run safely in sequence.",
        responsibilities: ["Priority 0-1000", "Retry with backoff", "Session lock checks", "Child concurrency guardrails"],
    },
    {
        id: "session-memory",
        title: "Sessions and Memory",
        subtitle: "Conversation continuity, durable memories, proposals, plans, and transcript search",
        layer: "controller",
        tier: "optional",
        category: "State",
        accent: "#8250df",
        description: "SQLite-backed session and memory stores preserve context while memory governance keeps durable knowledge reviewable.",
        responsibilities: ["Session transcripts", "Autonomous plan state", "Reviewed memories", "Memory proposals"],
    },
    {
        id: "repository-security",
        title: "Repository Security",
        subtitle: "Threat models, finding discovery, validation, patch proposals, and PR linkage",
        layer: "controller",
        tier: "optional",
        category: "Security",
        accent: "#cf222e",
        description: "RepositoryScan resources fan out AI work for security reviews and persist findings, validation state, and patch proposals.",
        responsibilities: ["Scheduled scans", "Threat models", "Finding validation", "Patch proposal tasks"],
    },
    {
        id: "metrics-webhooks",
        title: "Metrics and Webhooks",
        subtitle: "Prometheus metrics, structured logs, optional tracing, and HTTP completion callbacks",
        layer: "controller",
        tier: "optional",
        category: "Operations",
        accent: "#0969da",
        description: "Operational hooks make task progress visible and let downstream systems react to completions.",
        responsibilities: ["Prometheus metrics", "Structured logs", "Completion notifications", "Optional OpenTelemetry"],
    },
    {
        id: "crd-store",
        title: "Orka CRDs",
        subtitle: "Kubernetes API as the desired-state source",
        layer: "kubernetes",
        tier: "core",
        category: "Declarative",
        accent: "#1f883d",
        description: "Custom resources are the contract between users, GitOps, and the controller.",
        responsibilities: ["Task", "Agent", "Tool", "Provider", "Skill", "RepositoryScan"],
    },
    {
        id: "jobs-pods",
        title: "Jobs and Pods",
        subtitle: "Native Kubernetes execution with scheduling, retry, and isolation",
        layer: "kubernetes",
        tier: "core",
        category: "Runtime",
        accent: "#2f81f7",
        description: "The controller turns runnable tasks into Kubernetes Jobs and hardened worker Pods.",
        responsibilities: ["Job creation", "Pod lifecycle", "Cluster scheduling", "Status observation"],
    },
    {
        id: "secrets-rbac",
        title: "Secrets and RBAC",
        subtitle: "Provider API keys, ServiceAccounts, scoped permissions, and namespace boundaries",
        layer: "kubernetes",
        tier: "core",
        category: "Security",
        accent: "#d1242f",
        description: "Credentials stay in Kubernetes Secrets and worker Pods receive only the access they need.",
        responsibilities: ["Provider secretRef", "Worker ServiceAccounts", "TokenReview", "Namespace isolation"],
    },
    {
        id: "sqlite-store",
        title: "SQLite Stores",
        subtitle: "Results, sessions, plans, artifacts, memories, messages, and security scan data",
        layer: "kubernetes",
        tier: "core",
        category: "State",
        accent: "#6f42c1",
        description: "Embedded SQLite keeps Orka lightweight while preserving task outputs and operational history.",
        responsibilities: ["Results", "Artifacts up to 10 MB", "Session messages", "Security findings"],
    },
    {
        id: "general-worker",
        title: "General Worker",
        subtitle: "Runs arbitrary container commands for non-AI workloads",
        layer: "workers",
        tier: "core",
        category: "Runtime",
        accent: "#57606a",
        description: "The container worker handles general command execution through the same Task and Job lifecycle.",
        responsibilities: ["Container commands", "Result upload", "Logs", "Artifacts"],
    },
    {
        id: "ai-worker",
        title: "AI Worker",
        subtitle: "LLM agent runtime with built-in tools, skills, memory, transcript, and chat tools",
        layer: "workers",
        tier: "optional",
        category: "AI",
        accent: "#8250df",
        description: "The AI worker runs model-backed tasks and exposes the built-in tool surface used by orchestrator agents.",
        responsibilities: ["LLM prompts", "Tool execution", "Coordination tools", "Memory and transcript tools"],
    },
    {
        id: "agent-runtime-workers",
        title: "Agent Runtime Workers",
        subtitle: "Copilot CLI, Claude Code CLI, and Codex CLI workers for repo-backed tasks",
        layer: "workers",
        tier: "optional",
        category: "AI",
        accent: "#bf3989",
        description: "Specialized workers delegate tasks to external agent CLIs while preserving Kubernetes isolation and reporting.",
        responsibilities: ["Copilot CLI worker", "Claude Code worker", "Codex worker", "Repo-backed coding tasks"],
    },
    {
        id: "sandbox-workspaces",
        title: "Sandbox Workspaces",
        subtitle: "Experimental durable execution workspaces for agent runtimes",
        layer: "workers",
        tier: "optional",
        category: "Workspace",
        accent: "#0a7ea4",
        description: "Agent sandbox integration can provide reusable workspace execution for agent runtime tasks.",
        responsibilities: ["Durable workspaces", "Workspace staging", "Runtime isolation", "Experimental provider path"],
    },
    {
        id: "llm-providers",
        title: "LLM Providers",
        subtitle: "Anthropic, OpenAI, Azure OpenAI, and compatible endpoints",
        layer: "integrations",
        tier: "optional",
        category: "AI",
        accent: "#8250df",
        description: "Provider CRDs select model backends while credentials remain referenced through Kubernetes Secrets.",
        responsibilities: ["Provider CRD", "Streaming", "Fallback and cooldown", "Secret-backed API keys"],
    },
    {
        id: "tools-skills",
        title: "Tools and Skills",
        subtitle: "Built-in tools, HTTP Tool CRDs, and prompt Skill CRDs",
        layer: "integrations",
        tier: "optional",
        category: "Extension",
        accent: "#1f883d",
        description: "A layered capability system adds prompt content, built-in tools, and namespace-scoped custom HTTP tools.",
        responsibilities: ["Skill CRDs", "Built-in tools", "Custom Tool CRDs", "RBAC-controlled auth injection"],
    },
    {
        id: "github-workflows",
        title: "GitHub Workflows",
        subtitle: "Issues, PRs, review comments, CI checks, merge flows, and repository scans",
        layer: "integrations",
        tier: "optional",
        category: "DevOps",
        accent: "#57606a",
        description: "GitHub tools let agents create and review pull requests, inspect issues, and participate in repository security workflows.",
        responsibilities: ["Create PR", "Review PR", "Check CI", "Repository scan source"],
    },
    {
        id: "notification-targets",
        title: "Notification Targets",
        subtitle: "HTTP callbacks for completion events and downstream automation",
        layer: "integrations",
        tier: "optional",
        category: "Operations",
        accent: "#0969da",
        description: "Webhook callbacks let external systems react when tasks complete.",
        responsibilities: ["Completion webhooks", "Automation hooks", "Operational reporting", "Status propagation"],
    },
];

const edges = [
    { from: "web-dashboard", to: "api-chat", label: "UI + SSE" },
    { from: "api-clients", to: "api-chat", label: "REST / compat" },
    { from: "api-chat", to: "auth-middleware", label: "authorize" },
    { from: "auth-middleware", to: "reconcilers", label: "validated intent" },
    { from: "gitops-crds", to: "crd-store", label: "apply manifests" },
    { from: "crd-store", to: "reconcilers", label: "watch" },
    { from: "reconcilers", to: "priority-queue", label: "schedule" },
    { from: "priority-queue", to: "jobs-pods", label: "create Jobs" },
    { from: "reconcilers", to: "sqlite-store", label: "status + history" },
    { from: "session-memory", to: "sqlite-store", label: "persist context" },
    { from: "repository-security", to: "priority-queue", label: "fan out tasks" },
    { from: "repository-security", to: "github-workflows", label: "scan + PRs" },
    { from: "metrics-webhooks", to: "notification-targets", label: "callbacks" },
    { from: "jobs-pods", to: "general-worker", label: "container task" },
    { from: "jobs-pods", to: "ai-worker", label: "AI task" },
    { from: "jobs-pods", to: "agent-runtime-workers", label: "agent task" },
    { from: "agent-runtime-workers", to: "sandbox-workspaces", label: "workspace" },
    { from: "secrets-rbac", to: "jobs-pods", label: "SA + mounts" },
    { from: "secrets-rbac", to: "llm-providers", label: "secretRef" },
    { from: "ai-worker", to: "llm-providers", label: "complete/stream" },
    { from: "ai-worker", to: "tools-skills", label: "tool calls" },
    { from: "agent-runtime-workers", to: "github-workflows", label: "repo tasks" },
    { from: "general-worker", to: "api-chat", label: "results" },
    { from: "ai-worker", to: "api-chat", label: "results" },
    { from: "agent-runtime-workers", to: "api-chat", label: "results" },
];

const presets = [
    {
        id: "full-demo",
        title: "Full demo",
        description: "Everything visible for a complete architecture walkthrough.",
        enabled: components.map((component) => component.id),
    },
    {
        id: "minimal-platform",
        title: "Minimal platform",
        description: "Core API, CRDs, scheduling, storage, and container execution only.",
        enabled: components.filter((component) => component.tier === "core").map((component) => component.id),
    },
    {
        id: "ai-orchestration",
        title: "AI orchestration",
        description: "Enable chat, AI workers, agent runtime workers, memory, providers, tools, and webhooks.",
        enabled: [
            "web-dashboard",
            "api-clients",
            "gitops-crds",
            "api-chat",
            "auth-middleware",
            "reconcilers",
            "priority-queue",
            "session-memory",
            "metrics-webhooks",
            "crd-store",
            "jobs-pods",
            "secrets-rbac",
            "sqlite-store",
            "general-worker",
            "ai-worker",
            "agent-runtime-workers",
            "llm-providers",
            "tools-skills",
            "github-workflows",
            "notification-targets",
        ],
    },
    {
        id: "security-program",
        title: "Security program",
        description: "Show repository scanning, GitHub PR flows, durable history, and notifications.",
        enabled: [
            "web-dashboard",
            "api-clients",
            "gitops-crds",
            "api-chat",
            "auth-middleware",
            "reconcilers",
            "priority-queue",
            "session-memory",
            "repository-security",
            "metrics-webhooks",
            "crd-store",
            "jobs-pods",
            "secrets-rbac",
            "sqlite-store",
            "general-worker",
            "ai-worker",
            "llm-providers",
            "tools-skills",
            "github-workflows",
            "notification-targets",
        ],
    },
];

const defaultPresetId = "full-demo";
const defaultSelectedComponentId = "api-chat";
const defaultExcalidrawExportPath = join(homedir(), "orka-architecture.excalidraw");
const componentById = new Map(components.map((component) => [component.id, component]));
const layerById = new Map(layers.map((layer) => [layer.id, layer]));
const presetById = new Map(presets.map((preset) => [preset.id, preset]));
const servers = new Map();

function createInitialState() {
    const state = {
        enabled: {},
        presetId: defaultPresetId,
        selectedComponentId: defaultSelectedComponentId,
        updatedAt: new Date().toISOString(),
    };
    applyPresetToState(state, defaultPresetId);
    return state;
}

function applyPresetToState(state, presetId) {
    const preset = presetById.get(presetId);
    if (!preset) {
        throw new CanvasError("preset_not_found", `Unknown preset: ${presetId}`);
    }

    for (const component of components) {
        state.enabled[component.id] = false;
    }
    for (const componentId of preset.enabled) {
        if (!componentById.has(componentId)) {
            throw new CanvasError("preset_invalid", `Preset references unknown component: ${componentId}`);
        }
        state.enabled[componentId] = true;
    }

    state.presetId = preset.id;
    state.updatedAt = new Date().toISOString();
    if (!state.enabled[state.selectedComponentId]) {
        const firstEnabled = components.find((component) => state.enabled[component.id]);
        state.selectedComponentId = firstEnabled?.id ?? defaultSelectedComponentId;
    }
}

function setComponentEnabled(state, componentId, enabled) {
    if (!componentById.has(componentId)) {
        throw new CanvasError("component_not_found", `Unknown component: ${componentId}`);
    }
    state.enabled[componentId] = enabled === true;
    state.presetId = "custom";
    state.updatedAt = new Date().toISOString();
}

function selectComponent(state, componentId) {
    if (!componentById.has(componentId)) {
        throw new CanvasError("component_not_found", `Unknown component: ${componentId}`);
    }
    state.selectedComponentId = componentId;
    state.updatedAt = new Date().toISOString();
}

function applyOpenInput(state, input) {
    if (!input || typeof input !== "object") {
        return;
    }
    if (typeof input.preset === "string") {
        applyPresetToState(state, input.preset);
    }
    if (typeof input.selectedComponent === "string") {
        selectComponent(state, input.selectedComponent);
    }
}

function buildPayload(entry) {
    const enrichedComponents = components.map((component) => ({
        ...component,
        enabled: entry.state.enabled[component.id] !== false,
    }));
    const enabledCount = enrichedComponents.filter((component) => component.enabled).length;
    const optionalComponents = enrichedComponents.filter((component) => component.tier === "optional");
    const optionalEnabledCount = optionalComponents.filter((component) => component.enabled).length;

    return {
        meta: {
            title: "Orka Architecture",
            subtitle: "Kubernetes-native AI agent orchestration",
            summary: "Toggle optional capabilities to show how Orka can run as a small Kubernetes task platform or a full AI orchestration system.",
        },
        layers,
        components: enrichedComponents,
        edges,
        presets,
        state: {
            ...entry.state,
            enabled: { ...entry.state.enabled },
        },
        stats: {
            enabled: enabledCount,
            disabled: components.length - enabledCount,
            total: components.length,
            optionalEnabled: optionalEnabledCount,
            optionalTotal: optionalComponents.length,
        },
    };
}

function requireEntry(instanceId) {
    const entry = servers.get(instanceId);
    if (!entry) {
        throw new CanvasError("canvas_not_open", `Canvas instance is not open: ${instanceId}`);
    }
    return entry;
}

function escapeHtml(value) {
    return String(value)
        .replaceAll("&", "&amp;")
        .replaceAll("<", "&lt;")
        .replaceAll(">", "&gt;")
        .replaceAll('"', "&quot;")
        .replaceAll("'", "&#39;");
}

function wrapText(value, maxLength) {
    const words = String(value).split(/\s+/).filter(Boolean);
    const lines = [];
    let current = "";

    for (const word of words) {
        const candidate = current ? `${current} ${word}` : word;
        if (candidate.length > maxLength && current) {
            lines.push(current);
            current = word;
        } else {
            current = candidate;
        }
    }

    if (current) {
        lines.push(current);
    }

    return lines.length ? lines : [""];
}

function hashSeed(value) {
    let hash = 2166136261;
    for (const char of String(value)) {
        hash ^= char.charCodeAt(0);
        hash = Math.imul(hash, 16777619);
    }
    return hash >>> 0;
}

function elementId(prefix, value) {
    return `${prefix}_${String(value).replaceAll(/[^a-zA-Z0-9]+/g, "_")}`.slice(0, 48);
}

const flowStyles = {
    access: { id: "access", label: "Client input", color: "#0969da" },
    control: { id: "control", label: "Control plane", color: "#fb8500" },
    execution: { id: "execution", label: "Kubernetes execution", color: "#1f883d" },
    state: { id: "state", label: "State/results", color: "#8250df" },
    integration: { id: "integration", label: "External integrations", color: "#bf3989" },
    security: { id: "security", label: "Auth/secrets", color: "#d1242f" },
};

function flowStyleForEdge(edge) {
    const label = edge.label.toLowerCase();
    if (label.includes("authorize") || label.includes("secret") || label.includes("sa +")) {
        return flowStyles.security;
    }
    if (label.includes("results") || label.includes("status") || label.includes("persist")) {
        return flowStyles.state;
    }
    if (label.includes("create jobs") || label.includes("container task") || label.includes("ai task") || label.includes("agent task") || label.includes("workspace")) {
        return flowStyles.execution;
    }
    if (label.includes("complete") || label.includes("tool") || label.includes("repo") || label.includes("scan") || label.includes("callback")) {
        return flowStyles.integration;
    }
    if (label.includes("ui") || label.includes("rest") || label.includes("apply") || label.includes("watch")) {
        return flowStyles.access;
    }
    return flowStyles.control;
}

function buildArchitectureLayout(payload) {
    const primaryLayerIds = ["entry", "controller", "kubernetes", "workers"];
    const card = { width: 250, height: 112 };
    const frame = { x: 60, width: 900, pad: 28, header: 62 };
    const side = { x: 1000, y: 0, width: 390, pad: 28, header: 62 };
    const gap = { x: 22, y: 18, layer: 36 };
    const positioned = [];
    const positions = new Map();
    const frames = [];
    const enabled = payload.components.filter((component) => component.enabled);
    let y = 84;

    function addFrame(layerId, layerComponents, frameX, frameY, frameWidth, columns) {
        const layer = layerById.get(layerId);
        const rows = Math.max(1, Math.ceil(layerComponents.length / columns));
        const height = frame.header + rows * card.height + (rows - 1) * gap.y + frame.pad;
        const diagramFrame = {
            id: layerId,
            title: layer?.title ?? layerId,
            description: layer?.description ?? "",
            x: frameX,
            y: frameY,
            width: frameWidth,
            height,
        };
        frames.push(diagramFrame);

        for (const [index, component] of layerComponents.entries()) {
            const row = Math.floor(index / columns);
            const column = index % columns;
            const componentWidth = columns === 1 ? frameWidth - frame.pad * 2 : card.width;
            const node = {
                ...component,
                x: frameX + frame.pad + column * (card.width + gap.x),
                y: frameY + frame.header + row * (card.height + gap.y),
                width: componentWidth,
                height: card.height,
            };
            positioned.push(node);
            positions.set(component.id, node);
        }

        return height;
    }

    for (const layerId of primaryLayerIds) {
        const layerComponents = enabled.filter((component) => component.layer === layerId);
        if (!layerComponents.length) {
            continue;
        }
        const height = addFrame(layerId, layerComponents, frame.x, y, frame.width, 3);
        y += height + gap.layer;
    }

    const integrationComponents = enabled.filter((component) => component.layer === "integrations");
    if (integrationComponents.length) {
        const controllerFrame = frames.find((candidate) => candidate.id === "controller");
        side.y = controllerFrame ? controllerFrame.y : 84;
        addFrame("integrations", integrationComponents, side.x, side.y, side.width, 1);
    }

    const layoutEdges = payload.edges
        .filter((edge) => positions.has(edge.from) && positions.has(edge.to))
        .map((edge, index) => {
            const from = positions.get(edge.from);
            const to = positions.get(edge.to);
            const style = flowStyleForEdge(edge);
            const geometry = edgeGeometry(from, to, index);
            return {
                ...edge,
                index: index + 1,
                code: `F${index + 1}`,
                flowId: style.id,
                flowLabel: style.label,
                color: style.color,
                ...geometry,
            };
        });

    const legend = Array.from(new Map(layoutEdges.map((edge) => [edge.flowId, {
        id: edge.flowId,
        label: edge.flowLabel,
        color: edge.color,
    }])).values());
    const frameBottom = frames.reduce((bottom, current) => Math.max(bottom, current.y + current.height), 0);
    const baseWidth = integrationComponents.length ? 1450 : 1040;
    const legendWidth = legend.length ? 60 + 150 + legend.length * 190 + 120 : baseWidth;
    const width = Math.max(baseWidth, legendWidth);
    const legendY = frameBottom + 32;
    const height = Math.max(620, legendY + 112);
    const preset = presetById.get(payload.state.presetId);

    return {
        title: "Orka Architecture",
        subtitle: "Enabled components only",
        presetTitle: preset?.title ?? "Custom selection",
        width,
        height,
        frames,
        components: positioned,
        edges: layoutEdges,
        legend,
        legendY,
        stats: payload.stats,
        empty: positioned.length === 0,
    };
}

function edgeGeometry(from, to, index) {
    const fromCenter = {
        x: from.x + from.width / 2,
        y: from.y + from.height / 2,
    };
    const toCenter = {
        x: to.x + to.width / 2,
        y: to.y + to.height / 2,
    };
    const laneOffset = ((index % 7) - 3) * 14;

    if (to.x > from.x + from.width + 20) {
        const sx = from.x + from.width;
        const sy = fromCenter.y;
        const tx = to.x;
        const ty = toCenter.y;
        const midX = (sx + tx) / 2 + laneOffset;
        const points = [{ x: sx, y: sy }, { x: midX, y: sy }, { x: midX, y: ty }, { x: tx, y: ty }];
        return edgeGeometryResult(points, midX, (sy + ty) / 2);
    }

    if (to.x + to.width + 20 < from.x) {
        const sx = from.x;
        const sy = fromCenter.y;
        const tx = to.x + to.width;
        const ty = toCenter.y;
        const midX = (sx + tx) / 2 + laneOffset;
        const points = [{ x: sx, y: sy }, { x: midX, y: sy }, { x: midX, y: ty }, { x: tx, y: ty }];
        return edgeGeometryResult(points, midX, (sy + ty) / 2);
    }

    if (toCenter.y >= fromCenter.y) {
        const sx = fromCenter.x;
        const sy = from.y + from.height;
        const tx = toCenter.x;
        const ty = to.y;
        const midY = (sy + ty) / 2 + laneOffset;
        const points = [{ x: sx, y: sy }, { x: sx, y: midY }, { x: tx, y: midY }, { x: tx, y: ty }];
        return edgeGeometryResult(points, (sx + tx) / 2 + 20, midY);
    }

    const sx = fromCenter.x;
    const sy = from.y;
    const tx = toCenter.x;
    const ty = to.y + to.height;
    const midY = (sy + ty) / 2 - laneOffset;
    const points = [{ x: sx, y: sy }, { x: sx, y: midY }, { x: tx, y: midY }, { x: tx, y: ty }];
    return edgeGeometryResult(points, (sx + tx) / 2 + 20, midY);
}

function edgeGeometryResult(points, lx, ly) {
    const first = points[0];
    const last = points[points.length - 1];
    return {
        sx: first.x,
        sy: first.y,
        tx: last.x,
        ty: last.y,
        lx,
        ly,
        points,
        path: pathFromPoints(points),
    };
}

function pathFromPoints(points) {
    return points
        .map((point, index) => `${index === 0 ? "M" : "L"} ${point.x} ${point.y}`)
        .join(" ");
}

function svgText(value, x, y, options = {}) {
    const lines = wrapText(value, options.maxLength ?? 28).slice(0, options.maxLines ?? 3);
    const className = options.className ? ` class="${options.className}"` : "";
    const lineHeight = options.lineHeight ?? 18;
    const tspans = lines
        .map((line, index) => `<tspan x="${x}" dy="${index === 0 ? 0 : lineHeight}">${escapeHtml(line)}</tspan>`)
        .join("");
    return `<text${className} x="${x}" y="${y}">${tspans}</text>`;
}

function renderArchitectureSvg(layout) {
    if (layout.empty) {
        return `<svg xmlns="http://www.w3.org/2000/svg" width="980" height="540" viewBox="0 0 980 540" role="img" aria-label="No enabled Orka architecture components">
  <rect width="980" height="540" rx="28" fill="#fffdf7" stroke="#d6cfc2" stroke-width="2"/>
  <text x="490" y="250" text-anchor="middle" fill="#34322e" font-family="Virgil, Comic Sans MS, Segoe Print, cursive" font-size="34">No components enabled</text>
  <text x="490" y="296" text-anchor="middle" fill="#6f6a60" font-family="Arial, sans-serif" font-size="18">Go back to the selector and enable the components you want in the demo.</text>
</svg>`;
    }

    const framesSvg = layout.frames
        .map((current) => {
            const title = svgText(current.title, current.x + 26, current.y + 37, { className: "frame-title", maxLength: 42, maxLines: 1 });
            return `<g>
  <rect class="rough-frame shadow" x="${current.x + 3}" y="${current.y + 4}" width="${current.width}" height="${current.height}" rx="26"/>
  <rect class="rough-frame" x="${current.x}" y="${current.y}" width="${current.width}" height="${current.height}" rx="26"/>
  ${title}
</g>`;
        })
        .join("");

    const edgesSvg = layout.edges
        .map((edge) => {
            const labelWidth = Math.max(120, Math.min(210, 34 + edge.label.length * 7));
            const labelX = edge.lx + 18;
            const labelY = edge.ly - 17;
            return `<g>
  <path class="edge-halo" d="${edge.path}"/>
  <path class="flow-edge" d="${edge.path}" stroke="${edge.color}" marker-end="url(#arrowhead-${edge.flowId})"/>
  <circle class="endpoint-dot" cx="${edge.sx}" cy="${edge.sy}" r="5" stroke="${edge.color}"/>
  <circle class="edge-number-dot" cx="${edge.lx}" cy="${edge.ly}" r="14" fill="${edge.color}"/>
  <text class="edge-number" x="${edge.lx}" y="${edge.ly + 4}" text-anchor="middle">${escapeHtml(edge.code)}</text>
  <rect class="edge-label-box" x="${labelX}" y="${labelY}" width="${labelWidth}" height="34" rx="17" stroke="${edge.color}"/>
  <text class="edge-text" x="${labelX + 14}" y="${edge.ly + 5}">${escapeHtml(edge.label)}</text>
</g>`;
        })
        .join("");

    const markerDefs = layout.legend
        .map((item) => `<marker id="arrowhead-${item.id}" markerWidth="18" markerHeight="18" refX="15" refY="9" orient="auto" markerUnits="userSpaceOnUse">
      <path d="M 0 0 L 18 9 L 0 18 z" fill="${item.color}"/>
    </marker>`)
        .join("");

    const legendSvg = layout.legend.length
        ? `<g class="flow-legend" transform="translate(60 ${layout.legendY})">
  <rect class="legend-box" x="0" y="0" width="${layout.width - 120}" height="72" rx="22"/>
  <text class="legend-title" x="24" y="29">Flow key</text>
  ${layout.legend.map((item, index) => {
        const x = 150 + index * 190;
        return `<g transform="translate(${x} 20)">
    <line x1="0" y1="14" x2="42" y2="14" stroke="${item.color}" stroke-width="4" marker-end="url(#arrowhead-${item.id})"/>
    <text class="legend-text" x="54" y="19">${escapeHtml(item.label)}</text>
  </g>`;
    }).join("")}
</g>`
        : "";

    const componentsSvg = layout.components
        .map((component) => {
            const title = svgText(component.title, component.x + 18, component.y + 34, { className: "component-title", maxLength: 23, maxLines: 2, lineHeight: 18 });
            const subtitle = svgText(component.subtitle, component.x + 18, component.y + 76, { className: "component-subtitle", maxLength: 32, maxLines: 2, lineHeight: 15 });
            return `<g>
  <rect class="component-shadow" x="${component.x + 4}" y="${component.y + 5}" width="${component.width}" height="${component.height}" rx="20"/>
  <rect class="component-box" x="${component.x}" y="${component.y}" width="${component.width}" height="${component.height}" rx="20" stroke="${component.accent}"/>
  <circle cx="${component.x + component.width - 26}" cy="${component.y + 25}" r="8" fill="${component.accent}"/>
  ${title}
  ${subtitle}
</g>`;
        })
        .join("");

    return `<svg xmlns="http://www.w3.org/2000/svg" width="${layout.width}" height="${layout.height}" viewBox="0 0 ${layout.width} ${layout.height}" role="img" aria-label="Orka architecture diagram">
  <defs>
    ${markerDefs}
    <filter id="pencil">
      <feTurbulence type="fractalNoise" baseFrequency="0.018" numOctaves="2" seed="9"/>
      <feDisplacementMap in="SourceGraphic" scale="0.7"/>
    </filter>
  </defs>
  <style>
    .rough-frame { fill: rgba(255, 255, 255, 0.72); stroke: #34322e; stroke-width: 2.2; filter: url(#pencil); }
    .shadow, .component-shadow { fill: rgba(31, 35, 40, 0.08); stroke: none; filter: none; }
    .frame-title { fill: #34322e; font-family: "Virgil", "Comic Sans MS", "Segoe Print", cursive; font-size: 24px; font-weight: 700; }
    .component-box { fill: #fffdf7; stroke-width: 2.4; filter: url(#pencil); }
    .component-title { fill: #24292f; font-family: "Virgil", "Comic Sans MS", "Segoe Print", cursive; font-size: 17px; font-weight: 700; }
    .component-subtitle { fill: #6f6a60; font-family: Arial, sans-serif; font-size: 12px; }
    .edge-halo { fill: none; stroke: #fffdf7; stroke-width: 10px; stroke-linecap: round; stroke-linejoin: round; opacity: 0.92; }
    .flow-edge { fill: none; stroke-width: 4px; stroke-linecap: round; stroke-linejoin: round; filter: drop-shadow(0 3px 4px rgba(52, 50, 46, 0.18)); }
    .endpoint-dot { fill: #fffdf7; stroke-width: 3px; }
    .edge-number-dot { filter: drop-shadow(0 2px 4px rgba(52, 50, 46, 0.18)); }
    .edge-number { fill: #ffffff; font-family: Arial, sans-serif; font-size: 10px; font-weight: 900; }
    .edge-label-box { fill: rgba(255, 253, 247, 0.94); stroke-width: 2px; }
    .edge-text { fill: #34322e; font-family: Arial, sans-serif; font-size: 12px; font-weight: 800; }
    .legend-box { fill: rgba(255, 255, 255, 0.78); stroke: #d8d0c3; stroke-width: 2px; }
    .legend-title { fill: #34322e; font-family: "Virgil", "Comic Sans MS", "Segoe Print", cursive; font-size: 22px; font-weight: 700; }
    .legend-text { fill: #34322e; font-family: Arial, sans-serif; font-size: 12px; font-weight: 800; }
  </style>
  <rect width="${layout.width}" height="${layout.height}" rx="0" fill="#fffdf7"/>
  <text x="60" y="48" fill="#24292f" font-family="Virgil, Comic Sans MS, Segoe Print, cursive" font-size="34" font-weight="700">Orka Architecture</text>
  <text x="60" y="75" fill="#6f6a60" font-family="Arial, sans-serif" font-size="15">${escapeHtml(layout.presetTitle)} - ${layout.stats.enabled} enabled components</text>
  ${framesSvg}
  ${edgesSvg}
  ${componentsSvg}
  ${legendSvg}
</svg>`;
}

function excalidrawBase(id, type, x, y, width, height, strokeColor = "#34322e", backgroundColor = "transparent") {
    const seed = hashSeed(id);
    return {
        id,
        type,
        x,
        y,
        width,
        height,
        angle: 0,
        strokeColor,
        backgroundColor,
        fillStyle: "hachure",
        strokeWidth: 2,
        strokeStyle: "solid",
        roughness: 1,
        opacity: 100,
        groupIds: [],
        frameId: null,
        roundness: { type: 3 },
        seed,
        version: 1,
        versionNonce: seed,
        isDeleted: false,
        boundElements: null,
        updated: 1,
        link: null,
        locked: false,
    };
}

function excalidrawText(id, text, x, y, width, fontSize, color = "#24292f") {
    const lines = wrapText(text, Math.max(12, Math.floor(width / (fontSize * 0.56))));
    return {
        ...excalidrawBase(id, "text", x, y, width, lines.length * fontSize * 1.25, color, "transparent"),
        fontSize,
        fontFamily: 1,
        text: lines.join("\n"),
        rawText: lines.join("\n"),
        textAlign: "left",
        verticalAlign: "top",
        containerId: null,
        originalText: text,
        lineHeight: 1.25,
    };
}

function buildExcalidrawFile(layout) {
    const elements = [];

    elements.push(excalidrawText("title_orka_architecture", "Orka Architecture", 60, 30, 520, 34));
    elements.push(excalidrawText("subtitle_orka_architecture", `${layout.presetTitle} - ${layout.stats.enabled} enabled components`, 60, 74, 520, 16, "#6f6a60"));

    for (const current of layout.frames) {
        elements.push({
            ...excalidrawBase(elementId("frame", current.id), "rectangle", current.x, current.y, current.width, current.height, "#34322e", "#fffdf7"),
            strokeStyle: "dashed",
            roughness: 1.4,
        });
        elements.push(excalidrawText(elementId("frame_title", current.id), current.title, current.x + 26, current.y + 20, current.width - 52, 24));
    }

    for (const component of layout.components) {
        elements.push(excalidrawBase(elementId("component", component.id), "rectangle", component.x, component.y, component.width, component.height, component.accent, "#fffdf7"));
        elements.push(excalidrawText(elementId("component_title", component.id), component.title, component.x + 18, component.y + 18, component.width - 56, 18));
        elements.push(excalidrawText(elementId("component_subtitle", component.id), component.subtitle, component.x + 18, component.y + 66, component.width - 36, 12, "#6f6a60"));
    }

    for (const edge of layout.edges) {
        const minX = Math.min(...edge.points.map((point) => point.x));
        const minY = Math.min(...edge.points.map((point) => point.y));
        const maxX = Math.max(...edge.points.map((point) => point.x));
        const maxY = Math.max(...edge.points.map((point) => point.y));
        const points = edge.points.map((point) => [point.x - minX, point.y - minY]);
        elements.push({
            ...excalidrawBase(elementId("edge", `${edge.from}_${edge.to}`), "arrow", minX, minY, Math.max(1, maxX - minX), Math.max(1, maxY - minY), edge.color, "transparent"),
            roundness: { type: 2 },
            strokeWidth: 3,
            roughness: 0.6,
            points,
            lastCommittedPoint: null,
            startBinding: null,
            endBinding: null,
            startArrowhead: null,
            endArrowhead: "arrow",
        });
        elements.push(excalidrawText(elementId("edge_label", `${edge.from}_${edge.to}`), `${edge.code}: ${edge.label}`, edge.lx + 18, edge.ly - 16, 170, 12, edge.color));
    }

    for (const [index, item] of layout.legend.entries()) {
        const x = 60 + index * 220;
        const y = layout.legendY + 24;
        elements.push({
            ...excalidrawBase(elementId("legend_line", item.id), "arrow", x, y, 46, 0, item.color, "transparent"),
            strokeWidth: 3,
            points: [[0, 0], [46, 0]],
            lastCommittedPoint: null,
            startBinding: null,
            endBinding: null,
            startArrowhead: null,
            endArrowhead: "arrow",
        });
        elements.push(excalidrawText(elementId("legend_text", item.id), item.label, x + 58, y - 10, 150, 13, "#34322e"));
    }

    return {
        type: "excalidraw",
        version: 2,
        source: "https://excalidraw.com",
        elements,
        appState: {
            theme: "light",
            viewBackgroundColor: "#fffdf7",
            currentItemStrokeColor: "#34322e",
            currentItemBackgroundColor: "transparent",
            currentItemFillStyle: "hachure",
            currentItemStrokeWidth: 2,
            currentItemStrokeStyle: "solid",
            currentItemRoughness: 1,
            currentItemOpacity: 100,
            currentItemFontFamily: 1,
            currentItemFontSize: 20,
            scrollX: 0,
            scrollY: 0,
            zoom: { value: 0.72 },
            gridSize: null,
        },
        files: {},
    };
}

function buildExcalidrawResponse(entry) {
    const payload = buildPayload(entry);
    const layout = buildArchitectureLayout(payload);
    return {
        model: {
            title: layout.title,
            subtitle: layout.subtitle,
            presetTitle: layout.presetTitle,
            enabled: layout.stats.enabled,
            total: layout.stats.total,
            width: layout.width,
            height: layout.height,
            saveToken: entry.saveToken,
            svg: renderArchitectureSvg(layout),
        },
        file: buildExcalidrawFile(layout),
    };
}

async function saveExcalidrawFile(entry) {
    const response = buildExcalidrawResponse(entry);
    await writeFile(defaultExcalidrawExportPath, `${JSON.stringify(response.file, null, 2)}\n`, "utf8");
    return {
        path: defaultExcalidrawExportPath,
        bytes: Buffer.byteLength(JSON.stringify(response.file, null, 2), "utf8") + 1,
        file: response.file,
    };
}

function requireSaveToken(req, entry) {
    if (req.headers["x-orka-architecture-token"] !== entry.saveToken) {
        throw new CanvasError("save_token_invalid", "Refresh the diagram and try saving again.");
    }
}

function sendJson(res, status, payload) {
    res.writeHead(status, {
        "Content-Type": "application/json; charset=utf-8",
        "Cache-Control": "no-store",
    });
    res.end(JSON.stringify(payload));
}

function sendHtml(res, html) {
    res.writeHead(200, {
        "Content-Type": "text/html; charset=utf-8",
        "Cache-Control": "no-store",
    });
    res.end(html);
}

async function readJson(req) {
    const body = await new Promise((resolve, reject) => {
        let data = "";
        req.setEncoding("utf8");
        req.on("data", (chunk) => {
            data += chunk;
            if (data.length > 32768) {
                reject(new CanvasError("request_too_large", "Request body is too large."));
                req.destroy();
            }
        });
        req.on("end", () => resolve(data));
        req.on("error", reject);
    });

    if (!body) {
        return {};
    }

    try {
        return JSON.parse(body);
    } catch (error) {
        throw new CanvasError("invalid_json", error instanceof Error ? error.message : "Invalid JSON payload.");
    }
}

async function handleRequest(req, res, entry) {
    const url = new URL(req.url ?? "/", "http://127.0.0.1");
    const method = req.method ?? "GET";

    if (method === "GET" && url.pathname === "/") {
        sendHtml(res, renderHtml(entry.instanceId));
        return;
    }

    if (method === "GET" && url.pathname === "/excalidraw") {
        sendHtml(res, renderExcalidrawHtml(entry.instanceId));
        return;
    }

    if (method === "GET" && url.pathname === "/api/state") {
        sendJson(res, 200, buildPayload(entry));
        return;
    }

    if (method === "GET" && url.pathname === "/api/excalidraw") {
        sendJson(res, 200, buildExcalidrawResponse(entry));
        return;
    }

    if (method === "POST" && url.pathname === "/api/save-excalidraw") {
        requireSaveToken(req, entry);
        sendJson(res, 200, await saveExcalidrawFile(entry));
        return;
    }

    if (method === "POST" && url.pathname === "/api/component") {
        const input = await readJson(req);
        setComponentEnabled(entry.state, input.componentId, input.enabled);
        sendJson(res, 200, buildPayload(entry));
        return;
    }

    if (method === "POST" && url.pathname === "/api/select") {
        const input = await readJson(req);
        selectComponent(entry.state, input.componentId);
        sendJson(res, 200, buildPayload(entry));
        return;
    }

    if (method === "POST" && url.pathname === "/api/preset") {
        const input = await readJson(req);
        applyPresetToState(entry.state, input.presetId);
        sendJson(res, 200, buildPayload(entry));
        return;
    }

    if (method === "POST" && url.pathname === "/api/reset") {
        entry.state = createInitialState();
        sendJson(res, 200, buildPayload(entry));
        return;
    }

    throw new CanvasError("not_found", `No route for ${method} ${url.pathname}`);
}

function handleError(res, error) {
    const code = error instanceof CanvasError ? error.code : "request_failed";
    const status = code === "not_found" ? 404 : code === "request_failed" ? 500 : 400;
    const message = error instanceof Error ? error.message : String(error);
    sendJson(res, status, { error: { code, message } });
}

async function startServer(instanceId, input) {
    const entry = {
        instanceId,
        saveToken: randomUUID(),
        server: undefined,
        state: createInitialState(),
        url: undefined,
    };
    applyOpenInput(entry.state, input);

    const server = createServer((req, res) => {
        handleRequest(req, res, entry).catch((error) => handleError(res, error));
    });

    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    const port = typeof address === "object" && address ? address.port : 0;
    entry.server = server;
    entry.url = `http://127.0.0.1:${port}/`;
    return entry;
}

function renderExcalidrawHtml(instanceId) {
    const safeInstanceId = escapeHtml(instanceId);
    return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Orka Excalidraw Diagram</title>
  <style>
    :root {
      color-scheme: light;
      --paper: #fffdf7;
      --ink: #34322e;
      --muted: #6f6a60;
      --line: #d8d0c3;
      --accent: #0969da;
      --panel: rgba(255, 255, 255, 0.88);
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }

    * {
      box-sizing: border-box;
    }

    body {
      margin: 0;
      min-height: 100vh;
      color: var(--ink);
      background:
        radial-gradient(circle at 10% 8%, rgba(9, 105, 218, 0.12), transparent 28rem),
        radial-gradient(circle at 82% 14%, rgba(130, 80, 223, 0.12), transparent 30rem),
        var(--paper);
    }

    .toolbar {
      position: sticky;
      top: 0;
      z-index: 5;
      display: grid;
      grid-template-columns: minmax(0, 1fr) auto;
      gap: 18px;
      align-items: center;
      padding: 16px 20px;
      border-bottom: 1px solid var(--line);
      background: var(--panel);
      backdrop-filter: blur(16px);
      box-shadow: 0 10px 28px rgba(52, 50, 46, 0.08);
    }

    .title {
      min-width: 0;
    }

    h1 {
      margin: 0;
      font-family: "Virgil", "Comic Sans MS", "Segoe Print", cursive;
      font-size: clamp(24px, 3vw, 38px);
      line-height: 1;
      letter-spacing: -0.04em;
    }

    .meta {
      margin-top: 6px;
      color: var(--muted);
      font-size: 13px;
      line-height: 18px;
    }

    .actions {
      display: flex;
      flex-wrap: wrap;
      justify-content: flex-end;
      gap: 8px;
    }

    button,
    a.button {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      min-height: 38px;
      padding: 8px 12px;
      border: 1px solid var(--line);
      border-radius: 12px;
      background: #ffffff;
      color: var(--ink);
      box-shadow: 0 8px 18px rgba(52, 50, 46, 0.08);
      cursor: pointer;
      font: inherit;
      font-size: 13px;
      font-weight: 750;
      text-decoration: none;
    }

    button.primary {
      border-color: rgba(9, 105, 218, 0.28);
      background: #0969da;
      color: #ffffff;
    }

    .viewport {
      overflow: auto;
      min-height: calc(100vh - 86px);
      padding: 28px;
    }

    .paper {
      width: max-content;
      min-width: 100%;
      padding: 22px;
    }

    .paper svg {
      display: block;
      max-width: none;
      border: 1px solid var(--line);
      border-radius: 24px;
      box-shadow: 0 26px 80px rgba(52, 50, 46, 0.14);
      background: var(--paper);
    }

    .loading,
    .error {
      display: grid;
      min-height: 420px;
      place-items: center;
      color: var(--muted);
      font-weight: 750;
    }

    .toast {
      position: fixed;
      right: 20px;
      bottom: 20px;
      max-width: 360px;
      padding: 11px 13px;
      border: 1px solid var(--line);
      border-radius: 14px;
      background: #ffffff;
      box-shadow: 0 16px 44px rgba(52, 50, 46, 0.15);
      opacity: 0;
      transform: translateY(14px);
      pointer-events: none;
      transition: opacity 150ms ease, transform 150ms ease;
    }

    .toast.show {
      opacity: 1;
      transform: translateY(0);
    }

    @media print {
      .toolbar,
      .toast {
        display: none;
      }

      .viewport {
        padding: 0;
      }

      .paper {
        padding: 0;
      }

      .paper svg {
        border: 0;
        border-radius: 0;
        box-shadow: none;
      }
    }
  </style>
</head>
<body>
  <header class="toolbar">
    <div class="title">
      <h1>Orka Architecture</h1>
      <div class="meta" id="meta">Loading Excalidraw-style board for ${safeInstanceId}.</div>
    </div>
    <div class="actions">
      <a class="button" href="/" target="_self">Back to selector</a>
      <button type="button" id="refresh-button">Refresh from selector</button>
      <button type="button" id="save-local-button">Save local file</button>
      <button type="button" id="copy-button">Copy .excalidraw JSON</button>
      <button type="button" id="print-button">Print / save PDF</button>
      <button class="primary" type="button" id="download-button">Download .excalidraw</button>
    </div>
  </header>
  <main class="viewport">
    <div class="paper" id="paper"><div class="loading">Drawing selected components...</div></div>
  </main>
  <div class="toast" id="toast" role="status" aria-live="polite"></div>

  <script>
    let latest = null;

    function qs(selector) {
      return document.querySelector(selector);
    }

    function showToast(message) {
      const toast = qs("#toast");
      toast.textContent = message;
      toast.classList.add("show");
      window.setTimeout(function () {
        toast.classList.remove("show");
      }, 2600);
    }

    async function loadDiagram() {
      const response = await fetch("/api/excalidraw", { cache: "no-store" });
      const payload = await response.json();
      if (!response.ok) {
        const message = payload && payload.error ? payload.error.message : "Failed to load diagram";
        throw new Error(message);
      }
      latest = payload;
      qs("#meta").textContent = payload.model.presetTitle + " - " + payload.model.enabled + " of " + payload.model.total + " components included";
      qs("#paper").innerHTML = payload.model.svg;
    }

    function downloadExcalidraw() {
      if (!latest) {
        showToast("Diagram is still loading.");
        return;
      }
      const blob = new Blob([JSON.stringify(latest.file, null, 2)], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = "orka-architecture.excalidraw";
      document.body.append(link);
      link.click();
      link.remove();
      URL.revokeObjectURL(url);
      showToast("Downloaded orka-architecture.excalidraw");
    }

    async function saveExcalidrawLocal() {
      if (!latest) {
        showToast("Diagram is still loading.");
        return;
      }
      const response = await fetch("/api/save-excalidraw", {
        method: "POST",
        headers: { "X-Orka-Architecture-Token": latest.model.saveToken },
      });
      const payload = await response.json();
      if (!response.ok) {
        const message = payload && payload.error ? payload.error.message : "Failed to save file";
        throw new Error(message);
      }
      showToast("Saved to " + payload.path);
    }

    async function copyExcalidrawJson() {
      if (!latest) {
        showToast("Diagram is still loading.");
        return;
      }
      await navigator.clipboard.writeText(JSON.stringify(latest.file, null, 2));
      showToast("Copied Excalidraw JSON");
    }

    qs("#refresh-button").addEventListener("click", function () {
      loadDiagram().then(function () {
        showToast("Diagram refreshed");
      }).catch(function (error) {
        showToast(error.message);
      });
    });

    qs("#download-button").addEventListener("click", downloadExcalidraw);
    qs("#save-local-button").addEventListener("click", function () {
      saveExcalidrawLocal().catch(function (error) {
        showToast(error.message);
      });
    });
    qs("#copy-button").addEventListener("click", function () {
      copyExcalidrawJson().catch(function (error) {
        showToast(error.message);
      });
    });
    qs("#print-button").addEventListener("click", function () {
      window.print();
    });

    loadDiagram().catch(function (error) {
      const errorNode = document.createElement("div");
      errorNode.className = "error";
      errorNode.textContent = error.message;
      qs("#paper").replaceChildren(errorNode);
      showToast(error.message);
    });
  </script>
</body>
</html>`;
}

function renderHtml(instanceId) {
    const safeInstanceId = escapeHtml(instanceId);
    return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Orka Architecture</title>
  <style>
    :root {
      color-scheme: light dark;
      --canvas-bg: var(--background-color-default, #f6f8fa);
      --canvas-card: color-mix(in srgb, var(--background-color-default, #ffffff) 92%, transparent);
      --canvas-card-strong: color-mix(in srgb, var(--background-color-default, #ffffff) 82%, var(--color-white, #ffffff));
      --canvas-border: var(--border-color-default, #d0d7de);
      --canvas-text: var(--text-color-default, #1f2328);
      --canvas-muted: var(--text-color-muted, #656d76);
      --canvas-focus: var(--color-focus-outline, #0969da);
      --canvas-shadow: 0 24px 70px rgba(31, 35, 40, 0.14);
      --canvas-radius: 26px;
      --card-radius: 18px;
      --edge-color: rgba(84, 95, 111, 0.54);
      --edge-active: #0969da;
      font-family: var(--font-sans, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif);
    }

    * {
      box-sizing: border-box;
    }

    body {
      margin: 0;
      min-height: 100vh;
      background:
        radial-gradient(circle at 8% 4%, rgba(47, 129, 247, 0.22), transparent 28rem),
        radial-gradient(circle at 86% 8%, rgba(130, 80, 223, 0.2), transparent 30rem),
        radial-gradient(circle at 72% 78%, rgba(31, 136, 61, 0.16), transparent 28rem),
        var(--canvas-bg);
      color: var(--canvas-text);
      font-family: var(--font-sans, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif);
      font-size: var(--text-body-medium, 14px);
      line-height: var(--leading-body-medium, 20px);
    }

    button,
    input {
      font: inherit;
    }

    .shell {
      min-height: 100vh;
      padding: 24px;
    }

    .hero {
      position: relative;
      overflow: hidden;
      display: grid;
      grid-template-columns: minmax(0, 1fr) auto;
      gap: 18px;
      align-items: end;
      padding: 26px;
      border: 1px solid color-mix(in srgb, var(--canvas-border) 74%, transparent);
      border-radius: var(--canvas-radius);
      background:
        linear-gradient(135deg, rgba(47, 129, 247, 0.16), rgba(130, 80, 223, 0.1) 48%, rgba(31, 136, 61, 0.12)),
        var(--canvas-card);
      box-shadow: var(--canvas-shadow);
      backdrop-filter: blur(18px);
    }

    .hero:before {
      content: "";
      position: absolute;
      inset: -30% -20% auto auto;
      width: 420px;
      height: 420px;
      border-radius: 999px;
      background: radial-gradient(circle, rgba(255, 255, 255, 0.42), transparent 68%);
      pointer-events: none;
    }

    .eyebrow {
      display: inline-flex;
      width: fit-content;
      align-items: center;
      gap: 8px;
      padding: 7px 11px;
      border: 1px solid color-mix(in srgb, var(--canvas-border) 70%, transparent);
      border-radius: 999px;
      color: var(--canvas-muted);
      background: color-mix(in srgb, var(--canvas-card-strong) 72%, transparent);
      font-size: 12px;
      font-weight: 700;
      letter-spacing: 0.08em;
      text-transform: uppercase;
    }

    .pulse {
      width: 8px;
      height: 8px;
      border-radius: 999px;
      background: #1f883d;
      box-shadow: 0 0 0 6px rgba(31, 136, 61, 0.14);
    }

    h1 {
      margin: 14px 0 10px;
      font-family: var(--font-sans-display, var(--font-sans, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif));
      font-size: clamp(34px, 5vw, 64px);
      line-height: 0.96;
      letter-spacing: -0.06em;
    }

    .hero p {
      max-width: 820px;
      margin: 0;
      color: var(--canvas-muted);
      font-size: 16px;
      line-height: 24px;
    }

    .hero-stats {
      display: grid;
      grid-template-columns: repeat(3, minmax(94px, 1fr));
      gap: 10px;
      min-width: 330px;
    }

    .stat {
      padding: 14px;
      border: 1px solid color-mix(in srgb, var(--canvas-border) 70%, transparent);
      border-radius: 18px;
      background: color-mix(in srgb, var(--canvas-card-strong) 78%, transparent);
    }

    .stat-value {
      display: block;
      font-size: 26px;
      line-height: 30px;
      font-weight: 800;
      letter-spacing: -0.04em;
    }

    .stat-label {
      color: var(--canvas-muted);
      font-size: 12px;
      font-weight: 650;
      text-transform: uppercase;
      letter-spacing: 0.06em;
    }

    .main-grid {
      display: grid;
      grid-template-columns: minmax(0, 1fr) minmax(330px, 380px);
      gap: 18px;
      align-items: start;
      margin-top: 18px;
    }

    .diagram-board,
    .side-panel {
      border: 1px solid color-mix(in srgb, var(--canvas-border) 72%, transparent);
      border-radius: var(--canvas-radius);
      background:
        linear-gradient(180deg, color-mix(in srgb, var(--canvas-card-strong) 92%, transparent), color-mix(in srgb, var(--canvas-card) 88%, transparent)),
        var(--canvas-card);
      box-shadow: 0 18px 54px rgba(31, 35, 40, 0.1);
      backdrop-filter: blur(16px);
    }

    .diagram-board {
      position: relative;
      overflow: hidden;
      min-height: 760px;
      padding: 22px;
    }

    .diagram-board:before {
      content: "";
      position: absolute;
      inset: 0;
      background-image:
        linear-gradient(color-mix(in srgb, var(--canvas-border) 38%, transparent) 1px, transparent 1px),
        linear-gradient(90deg, color-mix(in srgb, var(--canvas-border) 38%, transparent) 1px, transparent 1px);
      background-size: 34px 34px;
      mask-image: linear-gradient(to bottom, rgba(0, 0, 0, 0.9), transparent 88%);
      opacity: 0.24;
      pointer-events: none;
    }

    .flow-svg,
    .edge-labels {
      position: absolute;
      inset: 0;
      width: 100%;
      height: 100%;
      pointer-events: none;
      z-index: 1;
    }

    .edge-path {
      fill: none;
      stroke: var(--edge-color);
      stroke-width: 2.2;
      stroke-linecap: round;
      opacity: 0.66;
      transition: opacity 160ms ease, stroke 160ms ease, stroke-width 160ms ease;
    }

    .edge-path.disabled {
      stroke-dasharray: 7 8;
      opacity: 0.16;
    }

    .edge-path.active {
      stroke: var(--edge-active);
      stroke-width: 3.4;
      opacity: 0.95;
      filter: drop-shadow(0 4px 8px rgba(9, 105, 218, 0.24));
    }

    .edge-label {
      position: absolute;
      transform: translate(-50%, -50%);
      max-width: 128px;
      padding: 4px 8px;
      border: 1px solid color-mix(in srgb, var(--canvas-border) 70%, transparent);
      border-radius: 999px;
      background: color-mix(in srgb, var(--canvas-card-strong) 86%, transparent);
      color: var(--canvas-muted);
      font-size: 11px;
      font-weight: 700;
      line-height: 15px;
      text-align: center;
      white-space: nowrap;
      box-shadow: 0 8px 20px rgba(31, 35, 40, 0.08);
    }

    .edge-label.disabled {
      opacity: 0.24;
    }

    .edge-label.active {
      border-color: color-mix(in srgb, var(--canvas-focus) 60%, var(--canvas-border));
      color: var(--canvas-focus);
    }

    .layers {
      position: relative;
      z-index: 2;
      display: grid;
      gap: 22px;
    }

    .layer {
      display: grid;
      grid-template-columns: minmax(150px, 210px) minmax(0, 1fr);
      gap: 16px;
      align-items: stretch;
    }

    .layer-info {
      padding: 14px 12px;
      border-left: 3px solid color-mix(in srgb, var(--canvas-focus) 58%, transparent);
    }

    .layer-info h2 {
      margin: 0 0 7px;
      font-size: 14px;
      line-height: 18px;
      text-transform: uppercase;
      letter-spacing: 0.08em;
    }

    .layer-info p {
      margin: 0;
      color: var(--canvas-muted);
      font-size: 12px;
      line-height: 17px;
    }

    .layer-grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(184px, 1fr));
      gap: 12px;
    }

    .component-card {
      position: relative;
      z-index: 2;
      display: grid;
      min-height: 136px;
      width: 100%;
      padding: 14px;
      border: 1px solid color-mix(in srgb, var(--accent) 34%, var(--canvas-border));
      border-radius: var(--card-radius);
      background:
        linear-gradient(145deg, color-mix(in srgb, var(--accent) 13%, var(--canvas-card-strong)), color-mix(in srgb, var(--canvas-card) 94%, transparent)),
        var(--canvas-card-strong);
      color: var(--canvas-text);
      text-align: left;
      box-shadow: 0 14px 30px rgba(31, 35, 40, 0.09);
      cursor: pointer;
      transition: transform 150ms ease, box-shadow 150ms ease, border-color 150ms ease, opacity 150ms ease, filter 150ms ease;
    }

    .component-card:hover {
      transform: translateY(-3px);
      box-shadow: 0 18px 42px rgba(31, 35, 40, 0.14);
    }

    .component-card.selected {
      border-color: var(--accent);
      box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 18%, transparent), 0 18px 44px rgba(31, 35, 40, 0.14);
    }

    .component-card.disabled {
      opacity: 0.46;
      filter: grayscale(0.75);
      background: color-mix(in srgb, var(--canvas-card) 92%, transparent);
      border-style: dashed;
    }

    .component-card h3 {
      margin: 10px 0 7px;
      font-size: 16px;
      line-height: 20px;
      letter-spacing: -0.02em;
    }

    .component-card p {
      margin: 0;
      color: var(--canvas-muted);
      font-size: 12px;
      line-height: 17px;
    }

    .card-top,
    .card-footer {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 8px;
    }

    .badge,
    .state-pill,
    .tiny-chip {
      display: inline-flex;
      align-items: center;
      width: fit-content;
      border-radius: 999px;
      font-weight: 800;
      letter-spacing: 0.05em;
      text-transform: uppercase;
      white-space: nowrap;
    }

    .badge {
      padding: 3px 7px;
      background: color-mix(in srgb, var(--accent) 15%, transparent);
      color: color-mix(in srgb, var(--accent) 82%, var(--canvas-text));
      font-size: 10px;
    }

    .state-pill {
      padding: 3px 7px;
      border: 1px solid color-mix(in srgb, var(--canvas-border) 80%, transparent);
      color: var(--canvas-muted);
      background: color-mix(in srgb, var(--canvas-card-strong) 72%, transparent);
      font-size: 10px;
    }

    .state-pill.on {
      color: #1f883d;
      border-color: rgba(31, 136, 61, 0.28);
      background: rgba(31, 136, 61, 0.1);
    }

    .state-pill.off {
      color: #cf222e;
      border-color: rgba(207, 34, 46, 0.24);
      background: rgba(207, 34, 46, 0.08);
    }

    .card-footer {
      align-self: end;
      margin-top: 13px;
    }

    .tiny-chip {
      padding: 3px 7px;
      border: 1px solid color-mix(in srgb, var(--canvas-border) 70%, transparent);
      color: var(--canvas-muted);
      background: color-mix(in srgb, var(--canvas-card-strong) 65%, transparent);
      font-size: 10px;
    }

    .side-panel {
      position: sticky;
      top: 18px;
      display: grid;
      gap: 14px;
      padding: 16px;
    }

    .panel-section {
      padding: 15px;
      border: 1px solid color-mix(in srgb, var(--canvas-border) 72%, transparent);
      border-radius: 20px;
      background: color-mix(in srgb, var(--canvas-card-strong) 72%, transparent);
    }

    .panel-section h2 {
      margin: 0 0 9px;
      font-size: 14px;
      line-height: 18px;
      letter-spacing: -0.01em;
    }

    .panel-section p {
      margin: 0;
      color: var(--canvas-muted);
      font-size: 12px;
      line-height: 17px;
    }

    .preset-grid {
      display: grid;
      gap: 8px;
    }

    .preset-button {
      width: 100%;
      padding: 11px 12px;
      border: 1px solid color-mix(in srgb, var(--canvas-border) 72%, transparent);
      border-radius: 15px;
      background: color-mix(in srgb, var(--canvas-card) 82%, transparent);
      color: var(--canvas-text);
      text-align: left;
      cursor: pointer;
      transition: border-color 150ms ease, transform 150ms ease, background 150ms ease;
    }

    .preset-button:hover {
      transform: translateY(-1px);
      border-color: color-mix(in srgb, var(--canvas-focus) 45%, var(--canvas-border));
    }

    .preset-button.active {
      border-color: var(--canvas-focus);
      background: color-mix(in srgb, var(--canvas-focus) 10%, var(--canvas-card-strong));
    }

    .preset-button strong {
      display: block;
      margin-bottom: 2px;
      font-size: 13px;
    }

    .preset-button span {
      display: block;
      color: var(--canvas-muted);
      font-size: 11px;
      line-height: 15px;
    }

    .toggle-group {
      margin-top: 13px;
    }

    .toggle-group:first-child {
      margin-top: 0;
    }

    .toggle-group-title {
      margin: 0 0 8px;
      color: var(--canvas-muted);
      font-size: 11px;
      font-weight: 800;
      letter-spacing: 0.07em;
      text-transform: uppercase;
    }

    .toggle-row {
      display: grid;
      grid-template-columns: 1fr auto;
      gap: 10px;
      align-items: center;
      padding: 9px 0;
      border-top: 1px solid color-mix(in srgb, var(--canvas-border) 54%, transparent);
    }

    .toggle-row:first-of-type {
      border-top: 0;
    }

    .toggle-title {
      display: block;
      font-size: 12px;
      font-weight: 750;
      line-height: 16px;
    }

    .toggle-meta {
      display: block;
      color: var(--canvas-muted);
      font-size: 11px;
      line-height: 15px;
    }

    .switch {
      position: relative;
      display: inline-block;
      width: 44px;
      height: 26px;
    }

    .switch input {
      width: 0;
      height: 0;
      opacity: 0;
    }

    .slider {
      position: absolute;
      inset: 0;
      border: 1px solid color-mix(in srgb, var(--canvas-border) 80%, transparent);
      border-radius: 999px;
      background: color-mix(in srgb, var(--canvas-muted) 18%, transparent);
      cursor: pointer;
      transition: 150ms ease;
    }

    .slider:before {
      content: "";
      position: absolute;
      width: 20px;
      height: 20px;
      left: 2px;
      top: 2px;
      border-radius: 50%;
      background: var(--color-white, #ffffff);
      box-shadow: 0 2px 8px rgba(31, 35, 40, 0.22);
      transition: transform 150ms ease;
    }

    .switch input:checked + .slider {
      border-color: rgba(31, 136, 61, 0.42);
      background: #1f883d;
    }

    .switch input:checked + .slider:before {
      transform: translateX(18px);
    }

    .inspector-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 12px;
      margin-bottom: 10px;
    }

    .inspector-title {
      margin: 0;
      font-size: 20px;
      line-height: 24px;
      letter-spacing: -0.04em;
    }

    .responsibility-list {
      display: flex;
      flex-wrap: wrap;
      gap: 7px;
      margin-top: 12px;
    }

    .connection-list {
      display: grid;
      gap: 7px;
      margin-top: 11px;
    }

    .connection {
      display: grid;
      grid-template-columns: auto 1fr;
      gap: 8px;
      align-items: center;
      padding: 8px;
      border: 1px solid color-mix(in srgb, var(--canvas-border) 62%, transparent);
      border-radius: 12px;
      background: color-mix(in srgb, var(--canvas-card) 76%, transparent);
      color: var(--canvas-muted);
      font-size: 11px;
      line-height: 15px;
    }

    .connection strong {
      color: var(--canvas-text);
      font-size: 12px;
    }

    .reset-button {
      width: 100%;
      padding: 10px 12px;
      border: 1px solid color-mix(in srgb, #cf222e 30%, var(--canvas-border));
      border-radius: 14px;
      background: rgba(207, 34, 46, 0.08);
      color: #cf222e;
      font-weight: 800;
      cursor: pointer;
    }

    .diagram-actions {
      background:
        linear-gradient(145deg, rgba(9, 105, 218, 0.12), rgba(130, 80, 223, 0.1)),
        color-mix(in srgb, var(--canvas-card-strong) 78%, transparent);
    }

    .popout-button,
    .modal-button {
      display: flex;
      align-items: center;
      justify-content: center;
      min-height: 44px;
      padding: 10px 12px;
      border: 1px solid color-mix(in srgb, var(--canvas-focus) 38%, var(--canvas-border));
      border-radius: 15px;
      background: var(--canvas-focus);
      color: var(--color-white, #ffffff);
      box-shadow: 0 14px 30px rgba(9, 105, 218, 0.18);
      font-weight: 850;
      text-decoration: none;
    }

    .popout-button {
      width: 100%;
      margin-top: 12px;
    }

    .popout-button:hover {
      filter: brightness(1.04);
    }

    .excalidraw-modal[hidden] {
      display: none;
    }

    .excalidraw-modal {
      position: fixed;
      inset: 0;
      z-index: 20;
      display: grid;
      padding: 22px;
      background: rgba(31, 35, 40, 0.46);
      backdrop-filter: blur(12px);
    }

    .modal-card {
      display: grid;
      grid-template-rows: auto minmax(0, 1fr);
      min-height: 0;
      overflow: hidden;
      border: 1px solid color-mix(in srgb, var(--canvas-border) 72%, transparent);
      border-radius: 26px;
      background: #fffdf7;
      box-shadow: 0 34px 100px rgba(31, 35, 40, 0.32);
      color: #34322e;
    }

    .modal-toolbar {
      display: grid;
      grid-template-columns: minmax(0, 1fr) auto;
      gap: 14px;
      align-items: center;
      padding: 16px 18px;
      border-bottom: 1px solid #d8d0c3;
      background: rgba(255, 255, 255, 0.82);
      backdrop-filter: blur(12px);
    }

    .modal-toolbar h2 {
      margin: 0;
      font-family: "Comic Sans MS", "Segoe Print", cursive;
      font-size: 26px;
      line-height: 30px;
      letter-spacing: -0.04em;
    }

    .modal-toolbar p {
      margin: 4px 0 0;
      color: #6f6a60;
      font-size: 12px;
      line-height: 17px;
    }

    .modal-actions {
      display: flex;
      flex-wrap: wrap;
      justify-content: flex-end;
      gap: 8px;
    }

    .modal-button {
      width: auto;
      min-height: 38px;
      border-color: #d8d0c3;
      background: #ffffff;
      color: #34322e;
      box-shadow: 0 8px 18px rgba(52, 50, 46, 0.08);
      cursor: pointer;
      font-size: 12px;
    }

    .modal-button.primary {
      border-color: rgba(9, 105, 218, 0.28);
      background: #0969da;
      color: #ffffff;
    }

    .modal-paper {
      min-height: 0;
      overflow: auto;
      padding: 22px;
      background:
        radial-gradient(circle at 12% 8%, rgba(9, 105, 218, 0.12), transparent 28rem),
        #fffdf7;
    }

    .modal-paper svg {
      display: block;
      max-width: none;
      border: 1px solid #d8d0c3;
      border-radius: 24px;
      background: #fffdf7;
      box-shadow: 0 24px 72px rgba(52, 50, 46, 0.14);
    }

    .toast {
      position: fixed;
      right: 22px;
      bottom: 22px;
      z-index: 30;
      max-width: 360px;
      padding: 12px 14px;
      border: 1px solid color-mix(in srgb, var(--canvas-border) 72%, transparent);
      border-radius: 16px;
      background: color-mix(in srgb, var(--canvas-card-strong) 94%, transparent);
      box-shadow: var(--canvas-shadow);
      color: var(--canvas-text);
      transform: translateY(18px);
      opacity: 0;
      pointer-events: none;
      transition: opacity 160ms ease, transform 160ms ease;
    }

    .toast.show {
      transform: translateY(0);
      opacity: 1;
    }

    .loading {
      display: grid;
      min-height: 420px;
      place-items: center;
      color: var(--canvas-muted);
      font-weight: 700;
    }

    @media (max-width: 1180px) {
      .hero,
      .main-grid {
        grid-template-columns: 1fr;
      }

      .hero-stats {
        min-width: 0;
      }

      .side-panel {
        position: static;
      }
    }

    @media (max-width: 760px) {
      .shell {
        padding: 14px;
      }

      .hero,
      .diagram-board,
      .side-panel {
        border-radius: 20px;
      }

      .hero-stats {
        grid-template-columns: 1fr;
      }

      .layer {
        grid-template-columns: 1fr;
      }

      .layer-info {
        border-left: 0;
        border-top: 3px solid color-mix(in srgb, var(--canvas-focus) 58%, transparent);
      }
    }
  </style>
</head>
<body>
  <main class="shell">
    <section class="hero">
      <div>
        <div class="eyebrow"><span class="pulse"></span> Interactive system map</div>
        <h1>Orka Architecture</h1>
        <p id="summary">Loading architecture model for instance ${safeInstanceId}.</p>
      </div>
      <div class="hero-stats" id="stats"></div>
    </section>

    <section class="main-grid">
      <div class="diagram-board" id="diagram-board">
        <svg class="flow-svg" id="edge-svg" aria-hidden="true"></svg>
        <div class="edge-labels" id="edge-labels" aria-hidden="true"></div>
        <div class="layers" id="layers"><div class="loading">Loading architecture...</div></div>
      </div>

      <aside class="side-panel" aria-label="Architecture controls">
        <section class="panel-section">
          <h2>Presentation presets</h2>
          <p>Switch between architecture slices during a demo.</p>
          <div class="preset-grid" id="presets"></div>
        </section>

        <section class="panel-section diagram-actions">
          <h2>Pop-out architecture diagram</h2>
          <p>Open a clean Excalidraw-style board using only the components currently enabled below.</p>
          <button class="popout-button" id="popout-button" type="button">Pop out Excalidraw diagram</button>
        </section>

        <section class="panel-section" id="inspector"></section>

        <section class="panel-section">
          <h2>Enable or disable components</h2>
          <p>Disabled components stay visible but fade out with dashed connections.</p>
          <div id="toggles"></div>
        </section>

        <section class="panel-section">
          <button class="reset-button" id="reset-button" type="button">Reset to full demo</button>
        </section>
      </aside>
    </section>
  </main>
  <div class="excalidraw-modal" id="excalidraw-modal" hidden>
    <div class="modal-card" role="dialog" aria-modal="true" aria-labelledby="excalidraw-modal-title">
      <header class="modal-toolbar">
        <div>
          <h2 id="excalidraw-modal-title">Orka Architecture Diagram</h2>
          <p id="excalidraw-modal-meta">Drawing selected components...</p>
        </div>
        <div class="modal-actions">
          <a class="modal-button" href="/excalidraw" target="_self">Open full page</a>
          <button class="modal-button" id="modal-refresh-button" type="button">Refresh</button>
          <button class="modal-button" id="modal-save-local-button" type="button">Save local file</button>
          <button class="modal-button" id="modal-copy-button" type="button">Copy JSON</button>
          <button class="modal-button" id="modal-print-button" type="button">Print</button>
          <button class="modal-button primary" id="modal-download-button" type="button">Download</button>
          <button class="modal-button" id="modal-close-button" type="button">Close</button>
        </div>
      </header>
      <div class="modal-paper" id="excalidraw-modal-paper"><div class="loading">Drawing selected components...</div></div>
    </div>
  </div>
  <div class="toast" id="toast" role="status" aria-live="polite"></div>

  <script>
    const app = {
      model: null,
      edgeFrame: 0,
    };

    function qs(selector, root) {
      return (root || document).querySelector(selector);
    }

    function el(tag, className, text) {
      const node = document.createElement(tag);
      if (className) {
        node.className = className;
      }
      if (text !== undefined) {
        node.textContent = text;
      }
      return node;
    }

    async function request(path, options) {
      const response = await fetch(path, Object.assign({
        headers: { "Content-Type": "application/json" },
      }, options || {}));
      const payload = await response.json();
      if (!response.ok) {
        const message = payload && payload.error ? payload.error.message : "Request failed";
        throw new Error(message);
      }
      return payload;
    }

    async function load() {
      app.model = await request("/api/state");
      render();
    }

    async function setComponent(componentId, enabled) {
      app.model = await request("/api/component", {
        method: "POST",
        body: JSON.stringify({ componentId: componentId, enabled: enabled }),
      });
      render();
    }

    async function applyPreset(presetId) {
      app.model = await request("/api/preset", {
        method: "POST",
        body: JSON.stringify({ presetId: presetId }),
      });
      render();
    }

    async function reset() {
      app.model = await request("/api/reset", { method: "POST" });
      render();
    }

    function showToast(message) {
      const toast = qs("#toast");
      toast.textContent = message;
      toast.classList.add("show");
      window.setTimeout(function () {
        toast.classList.remove("show");
      }, 2600);
    }

    function componentById(componentId) {
      return app.model.components.find(function (component) {
        return component.id === componentId;
      });
    }

    function selectedComponent() {
      return componentById(app.model.state.selectedComponentId) || app.model.components[0];
    }

    function render() {
      qs("#summary").textContent = app.model.meta.summary;
      renderStats();
      renderPresets();
      renderDiagram();
      renderInspector();
      renderToggles();
      scheduleEdges();
    }

    function renderStats() {
      const stats = qs("#stats");
      stats.replaceChildren(
        statNode(app.model.stats.enabled + "/" + app.model.stats.total, "Enabled"),
        statNode(String(app.model.stats.disabled), "Disabled"),
        statNode(app.model.stats.optionalEnabled + "/" + app.model.stats.optionalTotal, "Optional on"),
      );
    }

    function statNode(value, label) {
      const node = el("div", "stat");
      node.append(el("span", "stat-value", value), el("span", "stat-label", label));
      return node;
    }

    function renderPresets() {
      const presets = qs("#presets");
      presets.replaceChildren();

      for (const preset of app.model.presets) {
        const button = el("button", "preset-button" + (app.model.state.presetId === preset.id ? " active" : ""));
        button.type = "button";
        button.append(el("strong", "", preset.title), el("span", "", preset.description));
        button.addEventListener("click", function () {
          applyPreset(preset.id).catch(function (error) {
            showToast(error.message);
          });
        });
        presets.append(button);
      }
    }

    function renderDiagram() {
      const layers = qs("#layers");
      layers.replaceChildren();

      for (const layer of app.model.layers) {
        const section = el("section", "layer");
        const info = el("div", "layer-info");
        info.append(el("h2", "", layer.title), el("p", "", layer.description));

        const grid = el("div", "layer-grid");
        const layerComponents = app.model.components.filter(function (component) {
          return component.layer === layer.id;
        });

        for (const component of layerComponents) {
          grid.append(componentCard(component));
        }

        section.append(info, grid);
        layers.append(section);
      }
    }

    function componentCard(component) {
      const card = el("button", "component-card" + (component.enabled ? "" : " disabled") + (app.model.state.selectedComponentId === component.id ? " selected" : ""));
      card.type = "button";
      card.dataset.id = component.id;
      card.style.setProperty("--accent", component.accent);
      card.setAttribute("aria-pressed", app.model.state.selectedComponentId === component.id ? "true" : "false");

      const top = el("div", "card-top");
      top.append(el("span", "badge", component.tier), el("span", "state-pill " + (component.enabled ? "on" : "off"), component.enabled ? "Enabled" : "Disabled"));

      const title = el("h3", "", component.title);
      const subtitle = el("p", "", component.subtitle);

      const footer = el("div", "card-footer");
      footer.append(el("span", "tiny-chip", component.category), el("span", "tiny-chip", component.layer));

      card.append(top, title, subtitle, footer);
      card.addEventListener("click", function () {
        app.model.state.selectedComponentId = component.id;
        render();
        request("/api/select", {
          method: "POST",
          body: JSON.stringify({ componentId: component.id }),
        }).catch(function (error) {
          showToast(error.message);
        });
      });
      return card;
    }

    function renderInspector() {
      const component = selectedComponent();
      const inspector = qs("#inspector");
      inspector.replaceChildren();

      const header = el("div", "inspector-header");
      const titleWrap = el("div");
      titleWrap.append(el("h2", "inspector-title", component.title), el("p", "", component.subtitle));
      header.append(titleWrap, el("span", "state-pill " + (component.enabled ? "on" : "off"), component.enabled ? "Enabled" : "Disabled"));

      const description = el("p", "", component.description);
      const responsibilities = el("div", "responsibility-list");
      for (const item of component.responsibilities) {
        responsibilities.append(el("span", "tiny-chip", item));
      }

      const connections = el("div", "connection-list");
      const relatedEdges = app.model.edges.filter(function (edge) {
        return edge.from === component.id || edge.to === component.id;
      });

      if (relatedEdges.length === 0) {
        connections.append(el("p", "", "No direct connections in this view."));
      } else {
        for (const edge of relatedEdges) {
          const otherId = edge.from === component.id ? edge.to : edge.from;
          const other = componentById(otherId);
          const direction = edge.from === component.id ? "out" : "in";
          const row = el("div", "connection");
          row.append(el("span", "tiny-chip", direction), el("div", "", edge.label + " - " + (other ? other.title : otherId)));
          connections.append(row);
        }
      }

      inspector.append(header, description, responsibilities, el("h2", "", "Direct flows"), connections);
    }

    function renderToggles() {
      const toggles = qs("#toggles");
      toggles.replaceChildren();

      for (const layer of app.model.layers) {
        const group = el("div", "toggle-group");
        group.append(el("div", "toggle-group-title", layer.title));
        const layerComponents = app.model.components.filter(function (component) {
          return component.layer === layer.id;
        });

        for (const component of layerComponents) {
          const row = el("label", "toggle-row");
          const copy = el("span");
          copy.append(el("span", "toggle-title", component.title), el("span", "toggle-meta", component.tier + " - " + component.category));

          const switchWrap = el("span", "switch");
          const input = document.createElement("input");
          input.type = "checkbox";
          input.checked = component.enabled;
          input.addEventListener("change", function () {
            setComponent(component.id, input.checked).catch(function (error) {
              input.checked = !input.checked;
              showToast(error.message);
            });
          });
          switchWrap.append(input, el("span", "slider"));
          row.append(copy, switchWrap);
          group.append(row);
        }

        toggles.append(group);
      }
    }

    function scheduleEdges() {
      window.cancelAnimationFrame(app.edgeFrame);
      app.edgeFrame = window.requestAnimationFrame(drawEdges);
    }

    function drawEdges() {
      const board = qs("#diagram-board");
      const svg = qs("#edge-svg");
      const labels = qs("#edge-labels");
      const boardRect = board.getBoundingClientRect();
      const width = Math.max(1, boardRect.width);
      const height = Math.max(1, boardRect.height);
      const ns = "http://www.w3.org/2000/svg";

      svg.setAttribute("viewBox", "0 0 " + width + " " + height);
      svg.replaceChildren();
      labels.replaceChildren();

      const defs = document.createElementNS(ns, "defs");
      const marker = document.createElementNS(ns, "marker");
      marker.setAttribute("id", "arrow");
      marker.setAttribute("viewBox", "0 0 10 10");
      marker.setAttribute("refX", "8");
      marker.setAttribute("refY", "5");
      marker.setAttribute("markerWidth", "6");
      marker.setAttribute("markerHeight", "6");
      marker.setAttribute("orient", "auto-start-reverse");
      const arrow = document.createElementNS(ns, "path");
      arrow.setAttribute("d", "M 0 0 L 10 5 L 0 10 z");
      arrow.setAttribute("fill", "rgba(84, 95, 111, 0.58)");
      marker.append(arrow);
      defs.append(marker);
      svg.append(defs);

      for (const edge of app.model.edges) {
        const fromCard = qs('[data-id="' + edge.from + '"]');
        const toCard = qs('[data-id="' + edge.to + '"]');
        const from = componentById(edge.from);
        const to = componentById(edge.to);
        if (!fromCard || !toCard || !from || !to) {
          continue;
        }

        const fromRect = fromCard.getBoundingClientRect();
        const toRect = toCard.getBoundingClientRect();
        const x1 = fromRect.left - boardRect.left + fromRect.width / 2;
        const y1 = fromRect.bottom - boardRect.top - 4;
        const x2 = toRect.left - boardRect.left + toRect.width / 2;
        const y2 = toRect.top - boardRect.top + 4;
        const distance = Math.max(54, Math.abs(y2 - y1) * 0.48);
        const d = "M " + x1 + " " + y1 + " C " + x1 + " " + (y1 + distance) + " " + x2 + " " + (y2 - distance) + " " + x2 + " " + y2;
        const disabled = !from.enabled || !to.enabled;
        const active = app.model.state.selectedComponentId === edge.from || app.model.state.selectedComponentId === edge.to;

        const path = document.createElementNS(ns, "path");
        path.setAttribute("d", d);
        path.setAttribute("marker-end", "url(#arrow)");
        path.classList.add("edge-path");
        if (disabled) {
          path.classList.add("disabled");
        }
        if (active) {
          path.classList.add("active");
        }
        svg.append(path);

        const label = el("div", "edge-label" + (disabled ? " disabled" : "") + (active ? " active" : ""), edge.label);
        label.style.left = ((x1 + x2) / 2) + "px";
        label.style.top = ((y1 + y2) / 2) + "px";
        labels.append(label);
      }
    }

    let latestExcalidraw = null;

    async function loadExcalidrawModal() {
      const paper = qs("#excalidraw-modal-paper");
      paper.replaceChildren(el("div", "loading", "Drawing selected components..."));
      const payload = await request("/api/excalidraw");
      latestExcalidraw = payload;
      qs("#excalidraw-modal-meta").textContent = payload.model.presetTitle + " - " + payload.model.enabled + " of " + payload.model.total + " components included";
      paper.innerHTML = payload.model.svg;
    }

    function openExcalidrawModal() {
      const modal = qs("#excalidraw-modal");
      modal.hidden = false;
      document.body.style.overflow = "hidden";
      loadExcalidrawModal().catch(function (error) {
        qs("#excalidraw-modal-paper").replaceChildren(el("div", "loading", error.message));
        showToast(error.message);
      });
    }

    function closeExcalidrawModal() {
      qs("#excalidraw-modal").hidden = true;
      document.body.style.overflow = "";
    }

    function downloadExcalidraw() {
      if (!latestExcalidraw) {
        showToast("Diagram is still loading.");
        return;
      }
      const blob = new Blob([JSON.stringify(latestExcalidraw.file, null, 2)], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = "orka-architecture.excalidraw";
      document.body.append(link);
      link.click();
      link.remove();
      URL.revokeObjectURL(url);
      showToast("Downloaded orka-architecture.excalidraw");
    }

    async function saveExcalidrawLocal() {
      if (!latestExcalidraw) {
        showToast("Diagram is still loading.");
        return;
      }
      const payload = await request("/api/save-excalidraw", {
        method: "POST",
        headers: { "X-Orka-Architecture-Token": latestExcalidraw.model.saveToken },
      });
      showToast("Saved to " + payload.path);
    }

    async function copyExcalidrawJson() {
      if (!latestExcalidraw) {
        showToast("Diagram is still loading.");
        return;
      }
      await navigator.clipboard.writeText(JSON.stringify(latestExcalidraw.file, null, 2));
      showToast("Copied Excalidraw JSON");
    }

    qs("#popout-button").addEventListener("click", openExcalidrawModal);
    qs("#modal-refresh-button").addEventListener("click", function () {
      loadExcalidrawModal().then(function () {
        showToast("Diagram refreshed");
      }).catch(function (error) {
        showToast(error.message);
      });
    });
    qs("#modal-download-button").addEventListener("click", downloadExcalidraw);
    qs("#modal-save-local-button").addEventListener("click", function () {
      saveExcalidrawLocal().catch(function (error) {
        showToast(error.message);
      });
    });
    qs("#modal-copy-button").addEventListener("click", function () {
      copyExcalidrawJson().catch(function (error) {
        showToast(error.message);
      });
    });
    qs("#modal-print-button").addEventListener("click", function () {
      window.print();
    });
    qs("#modal-close-button").addEventListener("click", closeExcalidrawModal);
    qs("#excalidraw-modal").addEventListener("click", function (event) {
      if (event.target === event.currentTarget) {
        closeExcalidrawModal();
      }
    });
    document.addEventListener("keydown", function (event) {
      if (event.key === "Escape" && !qs("#excalidraw-modal").hidden) {
        closeExcalidrawModal();
      }
    });

    qs("#reset-button").addEventListener("click", function () {
      reset().catch(function (error) {
        showToast(error.message);
      });
    });

    window.addEventListener("resize", scheduleEdges);
    document.fonts.ready.then(scheduleEdges).catch(function () {
      scheduleEdges();
    });

    load().catch(function (error) {
      qs("#layers").replaceChildren(el("div", "loading", error.message));
      showToast(error.message);
    });
  </script>
</body>
</html>`;
}

const inputSchema = {
    type: "object",
    additionalProperties: false,
    properties: {
        preset: {
            type: "string",
            enum: presets.map((preset) => preset.id),
            description: "Initial demo preset to show.",
        },
        selectedComponent: {
            type: "string",
            enum: components.map((component) => component.id),
            description: "Component to highlight initially.",
        },
    },
};

const setComponentSchema = {
    type: "object",
    additionalProperties: false,
    required: ["componentId", "enabled"],
    properties: {
        componentId: {
            type: "string",
            enum: components.map((component) => component.id),
        },
        enabled: {
            type: "boolean",
        },
    },
};

const presetSchema = {
    type: "object",
    additionalProperties: false,
    required: ["presetId"],
    properties: {
        presetId: {
            type: "string",
            enum: presets.map((preset) => preset.id),
        },
    },
};

await joinSession({
    canvases: [
        createCanvas({
            id: "orka-architecture",
            displayName: "Orka Architecture",
            description: "Interactive Orka architecture diagram with demo presets, component toggles, and an Excalidraw-style pop-out.",
            inputSchema,
            actions: [
                {
                    name: "get_architecture_state",
                    description: "Return the current architecture diagram state, enabled components, and presets.",
                    handler: async (ctx) => buildPayload(requireEntry(ctx.instanceId)),
                },
                {
                    name: "set_component_enabled",
                    description: "Enable or disable a component in the architecture diagram.",
                    inputSchema: setComponentSchema,
                    handler: async (ctx) => {
                        const entry = requireEntry(ctx.instanceId);
                        const input = ctx.input ?? {};
                        setComponentEnabled(entry.state, input.componentId, input.enabled);
                        return buildPayload(entry);
                    },
                },
                {
                    name: "apply_preset",
                    description: "Apply a presentation preset to the architecture diagram.",
                    inputSchema: presetSchema,
                    handler: async (ctx) => {
                        const entry = requireEntry(ctx.instanceId);
                        const input = ctx.input ?? {};
                        applyPresetToState(entry.state, input.presetId);
                        return buildPayload(entry);
                    },
                },
                {
                    name: "get_excalidraw_export",
                    description: "Return an Excalidraw-compatible JSON export for the currently enabled components.",
                    handler: async (ctx) => buildExcalidrawResponse(requireEntry(ctx.instanceId)).file,
                },
                {
                    name: "save_excalidraw_file",
                    description: `Save the currently enabled components to ${defaultExcalidrawExportPath}.`,
                    handler: async (ctx) => saveExcalidrawFile(requireEntry(ctx.instanceId)),
                },
                {
                    name: "reset_architecture",
                    description: "Reset the architecture diagram to the full demo preset.",
                    handler: async (ctx) => {
                        const entry = requireEntry(ctx.instanceId);
                        entry.state = createInitialState();
                        return buildPayload(entry);
                    },
                },
            ],
            open: async (ctx) => {
                let entry = servers.get(ctx.instanceId);
                if (!entry) {
                    entry = await startServer(ctx.instanceId, ctx.input);
                    servers.set(ctx.instanceId, entry);
                } else {
                    applyOpenInput(entry.state, ctx.input);
                }
                return {
                    title: "Orka Architecture",
                    status: "Interactive architecture map",
                    url: entry.url,
                };
            },
            onClose: async (ctx) => {
                const entry = servers.get(ctx.instanceId);
                if (entry) {
                    servers.delete(ctx.instanceId);
                    await new Promise((resolve) => entry.server.close(() => resolve()));
                }
            },
        }),
    ],
});
