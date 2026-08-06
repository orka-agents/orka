import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";

const mockUseRepositoryScans = vi.fn();
const mockUseRunSecurityScan = vi.fn();

vi.mock("@tanstack/react-router", async () => {
  const actual = await vi.importActual("@tanstack/react-router");
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => {
      const { params, ...anchorProps } = props;
      void params;
      return (
        <a href={to} {...anchorProps}>
          {children}
        </a>
      );
    },
  };
});

vi.mock("@/hooks/use-security", () => ({
  useRepositoryScans: () => mockUseRepositoryScans(),
  useRunSecurityScan: (...args: unknown[]) => mockUseRunSecurityScan(...args),
}));

import { act, fireEvent, render, screen, within } from "@/test/test-utils";
import type { RepositoryScan, ScanRun } from "@/schemas/security";
import { RepositoryList } from "./repository-list";

type RepositoryQuality = NonNullable<
  NonNullable<RepositoryScan["status"]>["quality"]
>;

function currentQuality(
  overrides: Partial<RepositoryQuality> = {},
): RepositoryQuality {
  return {
    schemaVersion: 1,
    observedRepositoryScanUID: "repo-uid",
    observedGeneration: 3,
    inventoryCoverageStatus: "complete",
    candidateCoverageStatus: "complete",
    coverageStatus: "complete",
    validationScope: "all",
    validationExecution: "complete",
    attackPathExecution: "complete",
    analysisAttestationLevel: "tool-observed",
    targetVerification: "verified",
    bundleStatus: "not_started",
    authorizationStatus: "verified",
    isolationStatus: "hardened",
    ...overrides,
  };
}

type ScanRunQuality = NonNullable<ScanRun["quality"]>;

function scanRunQuality(
  quality: RepositoryQuality,
  overrides: Partial<ScanRunQuality> = {},
): ScanRunQuality {
  return {
    qualitySchemaVersion: quality.schemaVersion ?? 0,
    inventoryCoverageStatus: quality.inventoryCoverageStatus ?? "unknown",
    candidateCoverageStatus: quality.candidateCoverageStatus ?? "unknown",
    coverageStatus: quality.coverageStatus ?? "unknown",
    validationScope: quality.validationScope ?? "unknown",
    validationExecution: quality.validationExecution ?? "unknown",
    attackPathExecution: quality.attackPathExecution ?? "unknown",
    analysisAttestationLevel: quality.analysisAttestationLevel ?? "unverified",
    targetVerification: quality.targetVerification ?? "unverified",
    bundleStatus: quality.bundleStatus ?? "",
    authorizationStatus: quality.authorizationStatus ?? "legacy-unverified",
    isolationStatus: quality.isolationStatus ?? "unverified",
    reasonCodes: quality.reasonCodes,
    ...overrides,
  };
}

function repository(overrides: Partial<RepositoryScan> = {}): RepositoryScan {
  const base: RepositoryScan = {
    metadata: {
      name: "demo-repo",
      namespace: "default",
      uid: "repo-uid",
      generation: 3,
    },
    spec: {
      repoURL: "https://github.com/example/demo-repo",
      owner: "example",
      repository: "demo-repo",
      branch: "main",
      analysisAgentRef: { name: "security-agent" },
    },
    status: {
      phase: "Ready",
      lastScanID: "scan-current",
      findingCounts: { total: 0 },
    },
  };
  return {
    ...base,
    ...overrides,
    status:
      overrides.status === undefined
        ? base.status
        : { ...base.status, ...overrides.status },
  };
}

function latestScanRun(
  item: RepositoryScan,
  overrides: Partial<ScanRun> = {},
): ScanRun {
  const statusQuality = item.status?.quality;
  const phase =
    item.status?.phase === "Ready"
      ? "succeeded"
      : item.status?.phase === "Error"
        ? "failed"
        : "running";
  return {
    id: item.status?.lastScanID ?? "scan-current",
    runUID: "run-uid",
    namespace: item.metadata.namespace ?? "default",
    repositoryScan: item.metadata.name,
    repositoryScanUID: item.metadata.uid,
    repositoryScanGeneration: item.metadata.generation,
    taskName: "scan-task",
    mode: "manual",
    phase,
    startedAt: "2026-05-07T23:59:00Z",
    quality: statusQuality ? scanRunQuality(statusQuality) : undefined,
    ...overrides,
  };
}

function settledScanRuns(item: RepositoryScan, run = latestScanRun(item)) {
  return {
    data: { items: [run] },
    isLoading: false,
    isSuccess: true,
    isFetching: false,
    isError: false,
  };
}

function renderRepository(
  item: RepositoryScan,
  latestRunsResult: Record<string, any> = settledScanRuns(item),
  repositoryResultOverrides: Record<string, any> = {},
) {
  const latestScanRuns = latestRunsResult.data?.items;
  mockUseRepositoryScans.mockReturnValue({
    isLoading: false,
    isSuccess: latestRunsResult.isSuccess ?? true,
    isFetching: latestRunsResult.isFetching ?? false,
    isError: latestRunsResult.isError ?? false,
    dataUpdatedAt: latestRunsResult.dataUpdatedAt,
    data: {
      items: [item],
      ...(latestScanRuns === undefined ? {} : { latestScanRuns }),
    },
    ...repositoryResultOverrides,
  });
  return render(<RepositoryList />);
}

const currentQualityDimensionLabels = [
  "inventory: complete",
  "candidates: complete",
  "coverage: complete",
  "target: verified",
  "authorization: verified",
  "validation: all/complete",
  "attack path: complete",
  "attestation: tool observed",
  "isolation: hardened",
  "bundle: not started",
] as const;

function expectCurrentQualityDimensionsSuppressed() {
  for (const label of currentQualityDimensionLabels) {
    expect(screen.queryByLabelText(label)).not.toBeInTheDocument();
  }
}

describe("RepositoryList", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-05-08T00:00:00Z"));
    mockUseRepositoryScans.mockReset();
    mockUseRunSecurityScan.mockReset();
    mockUseRunSecurityScan.mockReturnValue({
      mutate: vi.fn(),
      reset: vi.fn(),
      isPending: false,
    });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("uses the batched latest runs for multiple repository cards", () => {
    const first = repository({
      metadata: { name: "repo-a", namespace: "default", uid: "uid-a", generation: 1 },
      spec: {
        repoURL: "https://github.com/example/repo-a",
        owner: "example",
        repository: "repo-a",
        branch: "main",
        analysisAgentRef: { name: "security-agent" },
      },
      status: {
        phase: "Ready",
        lastScanID: "scan-a",
        quality: currentQuality({ observedRepositoryScanUID: "uid-a", observedGeneration: 1 }),
      },
    });
    const second = repository({
      metadata: { name: "repo-b", namespace: "default", uid: "uid-b", generation: 2 },
      spec: {
        repoURL: "https://github.com/example/repo-b",
        owner: "example",
        repository: "repo-b",
        branch: "main",
        analysisAgentRef: { name: "security-agent" },
      },
      status: {
        phase: "Ready",
        lastScanID: "scan-b",
        quality: currentQuality({
          observedRepositoryScanUID: "uid-b",
          observedGeneration: 2,
          inventoryCoverageStatus: "partial",
        }),
      },
    });
    mockUseRepositoryScans.mockReturnValue({
      isLoading: false,
      isSuccess: true,
      isFetching: false,
      isError: false,
      data: {
        items: [first, second],
        latestScanRuns: [
          latestScanRun(second, { id: "scan-b" }),
          latestScanRun(first, { id: "scan-a" }),
          latestScanRun(first, { id: "scan-stale", repositoryScanUID: "old-uid" }),
        ],
      },
    });

    render(<RepositoryList />);

    const firstCard = screen.getByRole("link", { name: "repo-a" }).closest("[class*=transition-colors]");
    const secondCard = screen.getByRole("link", { name: "repo-b" }).closest("[class*=transition-colors]");
    expect(firstCard).not.toBeNull();
    expect(secondCard).not.toBeNull();
    expect(within(firstCard as HTMLElement).getByText("Quality current")).toBeInTheDocument();
    expect(within(secondCard as HTMLElement).getByText("Degraded")).toBeInTheDocument();
    expect(mockUseRunSecurityScan).toHaveBeenCalledTimes(2);
  });

  it("falls back to lastSuccessfulScanAt for repositories without lastScanAt", () => {
    renderRepository(
      repository({
        status: {
          phase: "Ready",
          lastSuccessfulScanAt: "2026-05-06T00:00:00Z",
        },
      }),
    );

    expect(screen.getByText("2d ago")).toBeInTheDocument();
    expect(screen.queryByText("Never")).not.toBeInTheDocument();
  });

  it("prefers lastScanAt when both scan timestamps are present", () => {
    renderRepository(
      repository({
        status: {
          phase: "Ready",
          lastScanAt: "2026-05-07T23:59:30Z",
          lastSuccessfulScanAt: "2026-05-06T00:00:00Z",
        },
      }),
    );

    expect(screen.getByText("30s ago")).toBeInTheDocument();
  });

  it("renders missing quality as unknown instead of healthy", () => {
    renderRepository(repository());

    expect(screen.getByText("Quality unknown")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "Latest stored scan run scan-current has no quality projection.",
    );
    expect(screen.queryByText("Quality current")).not.toBeInTheDocument();
  });

  it("renders quality as unverified when repository identity metadata is unavailable", () => {
    renderRepository(
      repository({
        metadata: { name: "demo-repo", namespace: "default", uid: "repo-uid" },
        status: { phase: "Ready", quality: currentQuality() },
      }),
    );

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "Repository UID or generation is unavailable",
    );
    expectCurrentQualityDimensionsSuppressed();
  });

  it.each([
    {
      name: "repository UID",
      quality: currentQuality({
        observedRepositoryScanUID: "previous-repo-uid",
      }),
      detail: "different repository UID",
    },
    {
      name: "repository generation",
      quality: currentQuality({ observedGeneration: 2 }),
      detail: "generation 2; current generation is 3",
    },
  ])(
    "renders stale $name status quality as unverified",
    ({ quality, detail }) => {
      const item = repository({ status: { phase: "Ready", quality } });
      renderRepository(
        item,
        settledScanRuns(item, latestScanRun(item, { quality: undefined })),
      );

      expect(screen.getByText("Quality unverified")).toBeInTheDocument();
      expect(screen.getByLabelText("Quality details")).toHaveTextContent(
        detail,
      );
      expect(screen.queryByText("Quality current")).not.toBeInTheDocument();
      expectCurrentQualityDimensionsSuppressed();
    },
  );

  it("renders a fresh healthy projection as current with concise dimension statuses", () => {
    renderRepository(
      repository({
        status: { phase: "Ready", quality: currentQuality() },
      }),
    );

    expect(screen.getByText("Quality current")).toBeInTheDocument();
    expect(screen.getByText("authorization: verified")).toBeInTheDocument();
    expect(screen.getByText("validation: all/complete")).toBeInTheDocument();
    expect(screen.getByText("attack path: complete")).toBeInTheDocument();
    expect(screen.getByText("isolation: hardened")).toBeInTheDocument();
    expect(screen.queryByText("Degraded")).not.toBeInTheDocument();
  });

  it.each(["not_started", "draft", "sealing", "sealed"])(
    "recognizes bundle status %s",
    (bundleStatus) => {
      renderRepository(
        repository({
          status: {
            phase: "Ready",
            quality: currentQuality({ bundleStatus }),
          },
        }),
      );

      expect(screen.getByText("Quality current")).toBeInTheDocument();
      expect(
        screen.getByLabelText(`bundle: ${bundleStatus.replace(/_/g, " ")}`),
      ).toBeInTheDocument();
      expect(screen.getByLabelText("Quality details")).not.toHaveTextContent(
        "unrecognized",
      );
    },
  );

  it.each([
    {
      name: "missing",
      bundleStatus: undefined,
      detail: "bundle status is missing; bundle integrity is unverified",
    },
    {
      name: "unknown",
      bundleStatus: "future_state",
      detail: "bundle status future state is unrecognized",
    },
  ])(
    "treats $name bundle status as degraded and unverified",
    ({ bundleStatus, detail }) => {
      renderRepository(
        repository({
          status: {
            phase: "Ready",
            quality: currentQuality({ bundleStatus }),
          },
        }),
      );

      expect(screen.getByText("Degraded")).toBeInTheDocument();
      expect(screen.getByLabelText("bundle: unverified")).toBeInTheDocument();
      expect(screen.getByLabelText("Quality details")).toHaveTextContent(
        detail,
      );
      expect(
        screen.queryByLabelText("bundle: not started"),
      ).not.toBeInTheDocument();
      expect(screen.queryByText("Quality current")).not.toBeInTheDocument();
    },
  );

  it("treats unsupported quality schemas as unverified", () => {
    renderRepository(
      repository({
        status: {
          phase: "Ready",
          quality: currentQuality({ schemaVersion: 2 }),
        },
      }),
    );

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "schema version 2",
    );
    expectCurrentQualityDimensionsSuppressed();
  });

  it.each([
    {
      name: "authorization",
      quality: currentQuality({ authorizationStatus: "legacy-unverified" }),
      status: "authorization: legacy unverified",
      detail: "authorization is legacy unverified",
    },
    {
      name: "validation",
      quality: currentQuality({ validationExecution: "partial" }),
      status: "validation: all/partial",
      detail: "validation is partial",
    },
    {
      name: "validation scope",
      quality: currentQuality({ validationScope: "unknown" }),
      status: "validation: unknown/complete",
      detail: "validation scope is unknown",
    },
    {
      name: "attack path",
      quality: currentQuality({ attackPathExecution: "failed" }),
      status: "attack path: failed",
      detail: "attack path is failed",
    },
    {
      name: "deferred attack path for full validation",
      quality: currentQuality({ attackPathExecution: "deferred" }),
      status: "attack path: deferred",
      detail: "attack path is deferred",
    },
    {
      name: "analysis attestation",
      quality: currentQuality({ analysisAttestationLevel: "delivered" }),
      status: "attestation: delivered",
      detail: "analysis attestation is delivered",
    },
    {
      name: "inventory coverage",
      quality: currentQuality({ inventoryCoverageStatus: "partial" }),
      status: "inventory: partial",
      detail: "inventory coverage is partial",
    },
    {
      name: "candidate coverage",
      quality: currentQuality({ candidateCoverageStatus: "partial" }),
      status: "candidates: partial",
      detail: "candidate coverage is partial",
    },
    {
      name: "isolation",
      quality: currentQuality({ isolationStatus: "fallback" }),
      status: "isolation: fallback",
      detail: "isolation is fallback",
    },
    {
      name: "terminal bundle failure",
      quality: currentQuality({ bundleStatus: "retryable_failed" }),
      status: "bundle: retryable failed",
      detail: "bundle is retryable failed",
    },
    {
      name: "permanent bundle failure",
      quality: currentQuality({ bundleStatus: "failed" }),
      status: "bundle: failed",
      detail: "bundle is failed",
    },
  ])("includes $name in degraded quality", ({ quality, status, detail }) => {
    renderRepository(repository({ status: { phase: "Ready", quality } }));

    expect(screen.getByText("Degraded")).toBeInTheDocument();
    expect(screen.getByLabelText(status)).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(detail);
  });

  it("fails closed when the repository query lags a newer succeeded run", () => {
    const item = repository({
      status: {
        phase: "Ready",
        lastScanID: "scan-old",
        quality: currentQuality(),
      },
    });
    const newerRun = latestScanRun(item, {
      id: "scan-newer",
      quality: scanRunQuality(
        currentQuality({ inventoryCoverageStatus: "partial" }),
      ),
    });
    renderRepository(item, settledScanRuns(item, newerRun));

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "Repository status is bound to run scan-old, not latest stored scan run scan-newer",
    );
    expectCurrentQualityDimensionsSuppressed();
  });

  it("fails closed when repository phase disagrees with the same succeeded run", () => {
    const item = repository({
      status: {
        phase: "Scanning",
        lastScanID: "scan-current",
        quality: currentQuality(),
      },
    });
    const succeededRun = latestScanRun(item, {
      phase: "succeeded",
      quality: scanRunQuality(currentQuality()),
    });
    renderRepository(item, settledScanRuns(item, succeededRun));

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "Repository status is Scanning, but latest stored scan run scan-current is succeeded",
    );
    expectCurrentQualityDimensionsSuppressed();
  });

  it("fails closed when the run query lags a newer Scanning repository status", () => {
    const item = repository({
      status: {
        phase: "Scanning",
        lastScanID: "scan-newer",
        quality: currentQuality(),
      },
    });
    const olderSucceededRun = latestScanRun(item, {
      id: "scan-older",
      phase: "succeeded",
      quality: scanRunQuality(currentQuality()),
    });
    renderRepository(item, settledScanRuns(item, olderSucceededRun));

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "Repository status is Scanning, but latest stored scan run scan-older is succeeded",
    );
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "Repository status is bound to run scan-newer, not latest stored scan run scan-older",
    );
    expectCurrentQualityDimensionsSuppressed();
  });

  it("rejects a retained status projection when the newer run has no quality", () => {
    const item = repository({
      status: {
        phase: "Ready",
        lastScanID: "scan-old",
        quality: currentQuality(),
      },
    });
    const newerRun = latestScanRun(item, {
      id: "scan-newer",
      quality: undefined,
    });
    renderRepository(item, settledScanRuns(item, newerRun));

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "Repository status is bound to run scan-old, not latest stored scan run scan-newer",
    );
    expectCurrentQualityDimensionsSuppressed();
  });

  it("does not present quality until latest run metadata is resolved", () => {
    renderRepository(
      repository({ status: { phase: "Ready", quality: currentQuality() } }),
      { data: undefined, isLoading: true },
    );

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "Latest scan run metadata is missing from the repository response",
    );
    expectCurrentQualityDimensionsSuppressed();
  });

  it("suppresses cached quality while a new scan mutation is pending", () => {
    const item = repository({
      status: { phase: "Ready", quality: currentQuality() },
    });
    mockUseRunSecurityScan.mockReturnValue({
      mutate: vi.fn(),
      reset: vi.fn(),
      isPending: true,
      isSuccess: false,
    });
    renderRepository(item);

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "new scan request is still pending",
    );
    expectCurrentQualityDimensionsSuppressed();
  });

  it("suppresses cached quality after a current-incarnation mutation error without disabling retry", () => {
    const submittedAt = Date.parse("2026-05-07T23:59:59Z");
    const cachedAt = submittedAt - 1;
    const item = repository({
      status: { phase: "Ready", quality: currentQuality() },
    });
    mockUseRunSecurityScan.mockReturnValue({
      mutate: vi.fn(),
      reset: vi.fn(),
      isPending: false,
      isSuccess: false,
      isError: true,
      submittedAt,
    });
    renderRepository(
      item,
      { ...settledScanRuns(item), dataUpdatedAt: cachedAt },
      { dataUpdatedAt: cachedAt },
    );

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "scan request failed after it may have reserved a run",
    );
    expectCurrentQualityDimensionsSuppressed();
    expect(screen.getByRole("button", { name: "Scan Now" })).toBeEnabled();
  });

  it("waits for a combined repository/latest-run observation made after a mutation error", () => {
    const failedAt = Date.parse("2026-05-08T00:00:00Z");
    const cachedAt = failedAt - 1;
    const freshAt = failedAt + 1;
    const item = repository({
      status: { phase: "Ready", quality: currentQuality() },
    });
    let mutationCallbacks: { onError?: () => void } | undefined;
    let mutationState: Record<string, unknown> = {
      isPending: false,
      isSuccess: false,
      isError: false,
      submittedAt: 0,
    };
    const mutate = vi.fn(
      (_variables: undefined, callbacks?: { onError?: () => void }) => {
        mutationCallbacks = callbacks;
      },
    );
    mockUseRunSecurityScan.mockImplementation(() => ({
      mutate,
      reset: vi.fn(),
      ...mutationState,
    }));
    const { rerender } = renderRepository(
      item,
      { ...settledScanRuns(item), dataUpdatedAt: cachedAt },
      { dataUpdatedAt: cachedAt },
    );

    fireEvent.click(screen.getByRole("button", { name: "Scan Now" }));
    mutationState = {
      isPending: false,
      isSuccess: false,
      isError: true,
      submittedAt: failedAt - 100,
    };
    act(() => mutationCallbacks?.onError?.());
    expect(screen.getByText("Quality unverified")).toBeInTheDocument();

    mockUseRepositoryScans.mockReturnValue({
      isLoading: false,
      isSuccess: true,
      isFetching: false,
      isError: false,
      dataUpdatedAt: freshAt,
      data: { items: [item], latestScanRuns: [latestScanRun(item)] },
    });
    rerender(<RepositoryList />);

    expect(screen.getByText("Quality current")).toBeInTheDocument();
    expect(screen.getByLabelText("inventory: complete")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Scan Now" })).toBeEnabled();
  });

  it("lets an idempotent retry success resolve a prior mutation error gate", () => {
    const failedAt = Date.parse("2026-05-08T00:00:00Z");
    const cachedAt = failedAt - 1;
    const item = repository({
      status: { phase: "Ready", quality: currentQuality() },
    });
    let mutationCallbacks: { onError?: () => void } | undefined;
    let mutationState: Record<string, unknown> = {
      isPending: false,
      isSuccess: false,
      isError: false,
      submittedAt: 0,
    };
    const mutate = vi.fn(
      (_variables: undefined, callbacks?: { onError?: () => void }) => {
        mutationCallbacks = callbacks;
      },
    );
    mockUseRunSecurityScan.mockImplementation(() => ({
      mutate,
      reset: vi.fn(),
      ...mutationState,
    }));
    const { rerender } = renderRepository(
      item,
      { ...settledScanRuns(item), dataUpdatedAt: cachedAt },
      { dataUpdatedAt: cachedAt },
    );

    fireEvent.click(screen.getByRole("button", { name: "Scan Now" }));
    mutationState = {
      isPending: false,
      isSuccess: false,
      isError: true,
      submittedAt: failedAt - 100,
    };
    act(() => mutationCallbacks?.onError?.());
    expect(screen.getByText("Quality unverified")).toBeInTheDocument();

    mutationState = {
      isPending: false,
      isSuccess: true,
      isError: false,
      submittedAt: failedAt + 100,
      data: { id: "scan-current" },
    };
    rerender(<RepositoryList />);

    expect(screen.getByText("Quality current")).toBeInTheDocument();
    expect(screen.getByLabelText("inventory: complete")).toBeInTheDocument();
  });

  it("suppresses cached quality after scan acceptance until both queries observe the new run", () => {
    const item = repository({
      status: { phase: "Ready", quality: currentQuality() },
    });
    mockUseRunSecurityScan.mockReturnValue({
      mutate: vi.fn(),
      reset: vi.fn(),
      isPending: false,
      isSuccess: true,
      data: { id: "scan-newer" },
    });
    renderRepository(item);

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "Scan scan-newer was accepted; waiting for fresh repository and run metadata",
    );
    expectCurrentQualityDimensionsSuppressed();
    expect(screen.getByRole("button", { name: "Scan Now" })).toBeDisabled();
  });

  it.each([
    {
      name: "UID recreation",
      metadata: { uid: "repo-uid-recreated", generation: 1 },
    },
    {
      name: "generation change",
      metadata: { uid: "repo-uid", generation: 4 },
    },
  ])(
    "ignores a retained successful mutation after a repository $name",
    ({ metadata }) => {
      const original = repository({
        status: { phase: "Ready", quality: currentQuality() },
      });
      mockUseRunSecurityScan.mockReturnValue({
        mutate: vi.fn(),
        reset: vi.fn(),
        isPending: false,
        isSuccess: true,
        data: { id: "scan-newer" },
      });
      const { rerender } = renderRepository(original);

      expect(screen.getByRole("button", { name: "Scan Now" })).toBeDisabled();

      const current = repository({
        metadata: {
          ...original.metadata,
          ...metadata,
        },
        status: {
          phase: "Ready",
          lastScanID: "scan-current",
          quality: currentQuality({
            observedRepositoryScanUID: metadata.uid,
            observedGeneration: metadata.generation,
          }),
        },
      });
      mockUseRepositoryScans.mockReturnValue({
        isLoading: false,
        isSuccess: true,
        isFetching: false,
        isError: false,
        data: { items: [current], latestScanRuns: [latestScanRun(current)] },
      });
      rerender(<RepositoryList />);

      expect(screen.getByText("Quality current")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Scan Now" })).toBeEnabled();
    },
  );

  it("releases the mutation freshness gate after both queries observe the accepted run", () => {
    const item = repository({
      status: {
        phase: "Ready",
        lastScanID: "scan-newer",
        quality: currentQuality(),
      },
    });
    mockUseRunSecurityScan.mockReturnValue({
      mutate: vi.fn(),
      reset: vi.fn(),
      isPending: false,
      isSuccess: true,
      data: { id: "scan-newer" },
    });
    renderRepository(
      item,
      settledScanRuns(item, latestScanRun(item, { id: "scan-newer" })),
    );

    expect(screen.getByText("Quality current")).toBeInTheDocument();
    expect(screen.getByLabelText("inventory: complete")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Scan Now" })).toBeEnabled();
  });

  it.each([
    {
      name: "equal run timestamps",
      acceptedStartedAt: "2026-05-07T23:00:00.123456700Z",
      externalStartedAt: "2026-05-07T23:00:00.123456700Z",
    },
    {
      name: "run clock rollback",
      acceptedStartedAt: "2026-05-07T23:00:00.123456700Z",
      externalStartedAt: "2026-05-07T22:59:59.999999999Z",
    },
  ])(
    "does not let a retained accepted result block a freshly observed authoritative external run under $name",
    ({ acceptedStartedAt, externalStartedAt }) => {
      const item = repository({
        status: {
          phase: "Ready",
          lastScanID: "scan-external",
          quality: currentQuality(),
        },
      });
      mockUseRunSecurityScan.mockReturnValue({
        mutate: vi.fn(),
        reset: vi.fn(),
        isPending: false,
        isSuccess: true,
        data: {
          id: "scan-accepted",
          startedAt: acceptedStartedAt,
        },
      });
      const externalRun = latestScanRun(item, {
        id: "scan-external",
        startedAt: externalStartedAt,
      });
      const refreshingRuns = {
        ...settledScanRuns(item, externalRun),
        isFetching: true,
      };
      const { rerender } = renderRepository(item, refreshingRuns, {
        isFetching: true,
      });

      expect(screen.getByText("Quality unverified")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Scan Now" })).toBeDisabled();

      mockUseRepositoryScans.mockReturnValue({
        isLoading: false,
        isSuccess: true,
        isFetching: false,
        isError: false,
        data: { items: [item], latestScanRuns: [externalRun] },
      });
      rerender(<RepositoryList />);

      expect(screen.getByText("Quality current")).toBeInTheDocument();
      expect(screen.getByLabelText("Quality details")).toHaveTextContent(
        "latest stored scan run scan-external",
      );
      expect(screen.getByRole("button", { name: "Scan Now" })).toBeEnabled();
    },
  );

  it("treats cached repository data as unverified while refetching", () => {
    const item = repository({
      status: { phase: "Ready", quality: currentQuality() },
    });
    renderRepository(item, settledScanRuns(item), {
      isSuccess: true,
      isFetching: true,
      isError: false,
    });

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "Repository metadata is refreshing; cached repository status cannot verify freshness",
    );
    expectCurrentQualityDimensionsSuppressed();
  });

  it("treats cached repository data as unverified after a refetch error", () => {
    const item = repository({
      status: { phase: "Ready", quality: currentQuality() },
    });
    renderRepository(item, settledScanRuns(item), {
      isSuccess: false,
      isFetching: false,
      isError: true,
    });

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "Repository metadata refresh failed; cached repository status cannot verify freshness",
    );
    expectCurrentQualityDimensionsSuppressed();
  });

  it("treats cached latest-run data as unverified while refetching", () => {
    const item = repository({
      status: { phase: "Ready", quality: currentQuality() },
    });
    renderRepository(item, {
      data: { items: [latestScanRun(item)] },
      isLoading: false,
      isSuccess: true,
      isFetching: true,
      isError: false,
    });

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "Repository metadata is refreshing; cached repository status cannot verify freshness",
    );
    expect(screen.queryByText("Quality current")).not.toBeInTheDocument();
    expectCurrentQualityDimensionsSuppressed();
  });

  it("treats cached latest-run data as unverified after a refetch error", () => {
    const item = repository({
      status: { phase: "Ready", quality: currentQuality() },
    });
    renderRepository(item, {
      data: { items: [latestScanRun(item)] },
      isLoading: false,
      isSuccess: false,
      isFetching: false,
      isError: true,
    });

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "Repository metadata refresh failed; cached repository status cannot verify freshness",
    );
    expect(screen.queryByText("Quality current")).not.toBeInTheDocument();
    expectCurrentQualityDimensionsSuppressed();
  });

  it("treats a previous projection as unknown while a newer scan is running", () => {
    renderRepository(
      repository({ status: { phase: "Scanning", quality: currentQuality() } }),
    );

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "scan-current is running; final quality is not available",
    );
    expect(screen.queryByText("Quality current")).not.toBeInTheDocument();
  });

  it("does not reuse a retained projection after a failed rescan", () => {
    renderRepository(
      repository({ status: { phase: "Error", quality: currentQuality() } }),
    );

    expect(screen.getByText("Quality unverified")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "scan-current is failed; final quality is not available",
    );
    expect(screen.queryByText("Quality current")).not.toBeInTheDocument();
  });

  it("accepts deferred attack-path work only when validation is off", () => {
    renderRepository(
      repository({
        status: {
          phase: "Ready",
          quality: currentQuality({
            validationScope: "off",
            attackPathExecution: "deferred",
          }),
        },
      }),
    );

    expect(screen.getByText("Quality current")).toBeInTheDocument();
    expect(screen.queryByText("Degraded")).not.toBeInTheDocument();
  });

  it("renders stable quality reason codes as concise details", () => {
    renderRepository(
      repository({
        status: {
          phase: "Ready",
          quality: currentQuality({ reasonCodes: ["bundle_seal_failed"] }),
        },
      }),
    );

    expect(screen.getByText("Degraded")).toBeInTheDocument();
    expect(screen.getByLabelText("Quality details")).toHaveTextContent(
      "bundle seal failed",
    );
  });
});
