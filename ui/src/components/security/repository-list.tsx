import { useState } from "react";
import { Link } from "@tanstack/react-router";
import { Shield, Plus, RefreshCcw } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/layout/page-header";
import {
  useRepositoryScans,
  useRunSecurityScan,
} from "@/hooks/use-security";
import type { RepositoryScan, ScanRun } from "@/schemas/security";

type Quality = NonNullable<NonNullable<RepositoryScan["status"]>["quality"]>;
type BadgeVariant = "default" | "secondary" | "destructive" | "outline";

const recognizedBundleStatuses: ReadonlySet<string> = new Set([
  "not_started",
  "draft",
  "sealing",
  "sealed",
  "retryable_failed",
  "failed",
]);
const failedBundleStatuses: readonly string[] = ["failed", "retryable_failed"];

type QualityPresentation = {
  label: string;
  variant: BadgeVariant;
  details: string[];
  currentQuality?: Quality;
};

function timeAgo(ts?: string) {
  if (!ts) return "Never";
  const seconds = Math.max(
    0,
    Math.floor((Date.now() - new Date(ts).getTime()) / 1000),
  );
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
}

function displayStatus(value?: string, fallback = "unknown") {
  return (value?.trim() || fallback).replace(/[_-]+/g, " ");
}

function statusVariant(
  value: string | undefined,
  healthy: readonly string[],
  terminalFailures: readonly string[] = [],
): BadgeVariant {
  if (healthy.includes(value ?? "")) return "default";
  if (terminalFailures.includes(value ?? "")) return "destructive";
  return "outline";
}

function recognizedBundleStatus(value?: string) {
  const normalized = value?.trim();
  return normalized && recognizedBundleStatuses.has(normalized)
    ? normalized
    : undefined;
}

type LatestRunBinding = {
  state: "pending" | "resolved" | "unverified";
  run?: ScanRun;
  observedAt?: number;
  unavailableDetail?: string;
};

type MutationErrorFreshnessGate = {
  repositoryIncarnation: string;
  failedAt: number;
};

function qualityFromScanRun(run: ScanRun): Quality | undefined {
  const quality = run.quality;
  if (!quality) return undefined;

  return {
    schemaVersion: quality.qualitySchemaVersion,
    inventoryCoverageStatus: quality.inventoryCoverageStatus,
    candidateCoverageStatus: quality.candidateCoverageStatus,
    coverageStatus: quality.coverageStatus,
    validationScope: quality.validationScope,
    validationExecution: quality.validationExecution,
    attackPathExecution: quality.attackPathExecution,
    analysisAttestationLevel: quality.analysisAttestationLevel,
    targetVerification: quality.targetVerification,
    bundleStatus: quality.bundleStatus,
    authorizationStatus: quality.authorizationStatus,
    isolationStatus: quality.isolationStatus,
    reasonCodes: quality.reasonCodes,
  };
}

function qualityPresentation(
  repo: RepositoryScan,
  latestRunBinding: LatestRunBinding,
): QualityPresentation {
  const statusQuality = repo.status?.quality;
  if (latestRunBinding.state !== "resolved") {
    return {
      label:
        latestRunBinding.state === "unverified" || statusQuality
          ? "Quality unverified"
          : "Quality unknown",
      variant: "secondary",
      details: [
        latestRunBinding.unavailableDetail ??
          "Latest scan run metadata is not available yet.",
      ],
    };
  }

  const latestRun = latestRunBinding.run;
  if (!latestRun) {
    return {
      label: statusQuality ? "Quality unverified" : "Quality unknown",
      variant: "secondary",
      details: [
        statusQuality
          ? "No stored scan run is available to verify the status quality projection."
          : "No stored scan run has reported quality.",
      ],
    };
  }

  const currentUID = repo.metadata.uid?.trim();
  const currentGeneration = repo.metadata.generation;
  if (!currentUID || typeof currentGeneration !== "number") {
    return {
      label: "Quality unverified",
      variant: "secondary",
      details: [
        "Repository UID or generation is unavailable; latest-run quality cannot be verified.",
      ],
    };
  }

  const latestRunID = latestRun.id?.trim();
  const runRepositoryUID = latestRun.repositoryScanUID?.trim();
  const runRepositoryGeneration = latestRun.repositoryScanGeneration;
  const bindingDetails: string[] = [];
  if (!latestRunID) {
    bindingDetails.push("Latest scan run has no run ID.");
  }
  if (latestRun.repositoryScan?.trim() !== repo.metadata.name) {
    bindingDetails.push("Latest scan run belongs to a different repository.");
  }
  const currentNamespace = repo.metadata.namespace?.trim();
  if (currentNamespace && latestRun.namespace?.trim() !== currentNamespace) {
    bindingDetails.push("Latest scan run belongs to a different namespace.");
  }
  if (!runRepositoryUID) {
    bindingDetails.push("Latest scan run has no repository UID binding.");
  } else if (runRepositoryUID !== currentUID) {
    bindingDetails.push(
      "Latest scan run belongs to a different repository UID.",
    );
  }
  if (typeof runRepositoryGeneration !== "number") {
    bindingDetails.push(
      "Latest scan run has no repository generation binding.",
    );
  } else if (runRepositoryGeneration !== currentGeneration) {
    bindingDetails.push(
      `Latest scan run is for generation ${runRepositoryGeneration}; current generation is ${currentGeneration}.`,
    );
  }
  if (bindingDetails.length > 0) {
    return {
      label: "Quality unverified",
      variant: "secondary",
      details: bindingDetails,
    };
  }

  if (latestRun.phase !== "succeeded") {
    return {
      label: statusQuality ? "Quality unverified" : "Quality unknown",
      variant: "secondary",
      details: [
        `Latest stored scan run ${latestRunID} is ${displayStatus(latestRun.phase)}; final quality is not available.`,
      ],
    };
  }

  const projectedRunID = repo.status?.lastScanID?.trim();
  const projectionDetails: string[] = [];
  if (repo.status?.phase !== "Ready") {
    projectionDetails.push(
      `Repository status is ${displayStatus(repo.status?.phase, "pending")}, but latest stored scan run ${latestRunID} is succeeded.`,
    );
  }
  if (!projectedRunID) {
    projectionDetails.push("Repository status has no last scan ID binding.");
  } else if (projectedRunID !== latestRunID) {
    projectionDetails.push(
      `Repository status is bound to run ${projectedRunID}, not latest stored scan run ${latestRunID}.`,
    );
  }
  if (projectionDetails.length > 0) {
    return {
      label: "Quality unverified",
      variant: "secondary",
      details: projectionDetails,
    };
  }

  let quality = qualityFromScanRun(latestRun);
  let bindingDetail = `Quality comes from latest stored scan run ${latestRunID} and matches the current repository UID and generation.`;
  if (!quality) {
    if (!statusQuality) {
      return {
        label: "Quality unknown",
        variant: "secondary",
        details: [
          `Latest stored scan run ${latestRunID} has no quality projection.`,
        ],
      };
    }

    const observedUID = statusQuality.observedRepositoryScanUID?.trim();
    const observedGeneration = statusQuality.observedGeneration;
    const freshnessDetails: string[] = [];
    if (!observedUID) {
      freshnessDetails.push("Status quality has no observed repository UID.");
    } else if (observedUID !== currentUID) {
      freshnessDetails.push(
        "Status quality belongs to a different repository UID.",
      );
    }
    if (typeof observedGeneration !== "number") {
      freshnessDetails.push("Status quality has no observed generation.");
    } else if (observedGeneration !== currentGeneration) {
      freshnessDetails.push(
        `Status quality is for generation ${observedGeneration}; current generation is ${currentGeneration}.`,
      );
    }
    if (freshnessDetails.length > 0) {
      return {
        label: "Quality unverified",
        variant: "secondary",
        details: freshnessDetails,
      };
    }

    quality = statusQuality;
    bindingDetail = `Status quality is bound to latest stored scan run ${latestRunID} and matches the current repository UID and generation.`;
  }

  if (quality.schemaVersion !== 1) {
    return {
      label: "Quality unverified",
      variant: "secondary",
      details: [
        `Unsupported quality schema version ${quality.schemaVersion ?? "unknown"}.`,
      ],
    };
  }

  const degradationDetails: string[] = [];
  if (quality.inventoryCoverageStatus !== "complete") {
    degradationDetails.push(
      `inventory coverage is ${displayStatus(quality.inventoryCoverageStatus)}`,
    );
  }
  if (quality.candidateCoverageStatus !== "complete") {
    degradationDetails.push(
      `candidate coverage is ${displayStatus(quality.candidateCoverageStatus)}`,
    );
  }
  if (quality.coverageStatus !== "complete") {
    degradationDetails.push(
      `coverage is ${displayStatus(quality.coverageStatus)}`,
    );
  }
  if (quality.targetVerification !== "verified") {
    degradationDetails.push(
      `target is ${displayStatus(quality.targetVerification)}`,
    );
  }
  if (!["verified", "admitted"].includes(quality.authorizationStatus ?? "")) {
    degradationDetails.push(
      `authorization is ${displayStatus(quality.authorizationStatus)}`,
    );
  }
  if (quality.validationExecution !== "complete") {
    degradationDetails.push(
      `validation is ${displayStatus(quality.validationExecution)}`,
    );
  }
  if (!["off", "sampled", "all"].includes(quality.validationScope ?? "")) {
    degradationDetails.push(
      `validation scope is ${displayStatus(quality.validationScope)}`,
    );
  }
  const attackPathComplete =
    quality.attackPathExecution === "complete" ||
    (quality.validationScope === "off" &&
      quality.attackPathExecution === "deferred");
  if (!attackPathComplete) {
    degradationDetails.push(
      `attack path is ${displayStatus(quality.attackPathExecution)}`,
    );
  }
  if (
    !["tool-observed", "brokered"].includes(
      quality.analysisAttestationLevel ?? "",
    )
  ) {
    degradationDetails.push(
      `analysis attestation is ${displayStatus(quality.analysisAttestationLevel)}`,
    );
  }
  if (quality.isolationStatus !== "hardened") {
    degradationDetails.push(
      `isolation is ${displayStatus(quality.isolationStatus)}`,
    );
  }
  const bundleStatus = recognizedBundleStatus(quality.bundleStatus);
  if (!bundleStatus) {
    const suppliedBundleStatus = quality.bundleStatus?.trim();
    degradationDetails.push(
      suppliedBundleStatus
        ? `bundle status ${displayStatus(suppliedBundleStatus)} is unrecognized`
        : "bundle status is missing; bundle integrity is unverified",
    );
  } else if (failedBundleStatuses.includes(bundleStatus)) {
    degradationDetails.push(`bundle is ${displayStatus(bundleStatus)}`);
  }
  for (const reason of quality.reasonCodes ?? []) {
    const detail = displayStatus(reason, "");
    if (detail) degradationDetails.push(detail);
  }

  if (degradationDetails.length > 0) {
    return {
      label: "Degraded",
      variant: "destructive",
      details: [...Array.from(new Set(degradationDetails)), bindingDetail],
      currentQuality: quality,
    };
  }

  return {
    label: "Quality current",
    variant: "default",
    details: [bindingDetail],
    currentQuality: quality,
  };
}

function conciseDetails(details: string[]) {
  const visible = details.slice(0, 4);
  const remaining = details.length - visible.length;
  return `${visible.join(" · ")}${remaining > 0 ? ` · +${remaining} more` : ""}`;
}

function repositoryIncarnation(repo: RepositoryScan) {
  return JSON.stringify([
    repo.metadata.uid ?? null,
    repo.metadata.generation ?? null,
  ]);
}

function latestRunIncarnationKey(
  repositoryName: string,
  repositoryUID?: string,
  repositoryGeneration?: number,
) {
  const name = repositoryName.trim();
  const uid = repositoryUID?.trim();
  if (!name || !uid || typeof repositoryGeneration !== "number") {
    return undefined;
  }
  return JSON.stringify([name, uid, repositoryGeneration]);
}

export function RepositoryList() {
  const repositories = useRepositoryScans();
  const { data, isLoading } = repositories;
  const repositoryQueryIsSettled =
    repositories.isSuccess && !repositories.isFetching && !repositories.isError;
  const repositoryQueryHasCachedData = repositories.data !== undefined;
  const repositoryQueryBinding: LatestRunBinding = {
    state: repositoryQueryIsSettled
      ? "resolved"
      : repositoryQueryHasCachedData || repositories.isError
        ? "unverified"
        : "pending",
    observedAt: repositoryQueryIsSettled
      ? repositories.dataUpdatedAt
      : undefined,
    unavailableDetail: repositories.isError
      ? "Repository metadata refresh failed; cached repository status cannot verify freshness."
      : repositories.isFetching && repositoryQueryHasCachedData
        ? "Repository metadata is refreshing; cached repository status cannot verify freshness."
        : undefined,
  };
  const latestRunsByRepository = new Map<string, ScanRun>();
  for (const run of data?.latestScanRuns ?? []) {
    const key = latestRunIncarnationKey(
      run.repositoryScan,
      run.repositoryScanUID,
      run.repositoryScanGeneration,
    );
    if (key) latestRunsByRepository.set(key, run);
  }
  const latestRunQueryBinding: LatestRunBinding =
    repositoryQueryIsSettled && data?.latestScanRuns === undefined
      ? {
          state: "unverified",
          observedAt: repositories.dataUpdatedAt,
          unavailableDetail:
            "Latest scan run metadata is missing from the repository response.",
        }
      : repositoryQueryBinding;

  return (
    <div className="space-y-4">
      <PageHeader
        title="Security"
        description="Repository security scans, threat models, and findings"
        action={
          <Link to="/security/new">
            <Button>
              <Plus className="mr-2 h-4 w-4" />
              New Repository
            </Button>
          </Link>
        }
      />

      {isLoading ? (
        <div className="grid gap-4 md:grid-cols-2">
          {Array.from({ length: 4 }).map((_, index) => (
            <Card key={index}>
              <CardHeader>
                <Skeleton className="h-6 w-48" />
              </CardHeader>
              <CardContent className="space-y-2">
                <Skeleton className="h-4 w-full" />
                <Skeleton className="h-4 w-2/3" />
              </CardContent>
            </Card>
          ))}
        </div>
      ) : (data?.items ?? []).length === 0 ? (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground">
            No repositories configured for security scanning yet.
          </CardContent>
        </Card>
      ) : (
        <div className="grid gap-4 md:grid-cols-2">
          {(data?.items ?? []).map((repo) => {
            const key = latestRunIncarnationKey(
              repo.metadata.name,
              repo.metadata.uid,
              repo.metadata.generation,
            );
            return (
              <RepositoryCard
                key={repo.metadata.name}
                repo={repo}
                repositoryQueryBinding={repositoryQueryBinding}
                latestRunBinding={
                  latestRunQueryBinding.state === "resolved" && !key
                    ? {
                        state: "unverified",
                        observedAt: latestRunQueryBinding.observedAt,
                        unavailableDetail:
                          "Repository UID or generation is unavailable; latest-run quality cannot be verified.",
                      }
                    : {
                        ...latestRunQueryBinding,
                        run:
                          latestRunQueryBinding.state === "resolved" && key
                            ? latestRunsByRepository.get(key)
                            : undefined,
                      }
                }
              />
            );
          })}
        </div>
      )}
    </div>
  );
}

function RepositoryCard({
  repo,
  repositoryQueryBinding,
  latestRunBinding,
}: {
  repo: RepositoryScan;
  repositoryQueryBinding: LatestRunBinding;
  latestRunBinding: LatestRunBinding;
}) {
  const runScan = useRunSecurityScan(repo.metadata.name);
  const currentRepositoryIncarnation = repositoryIncarnation(repo);
  const [mutationRepositoryIncarnation, setMutationRepositoryIncarnation] =
    useState(currentRepositoryIncarnation);
  const [mutationErrorFreshnessGate, setMutationErrorFreshnessGate] =
    useState<MutationErrorFreshnessGate>();
  const mutationStateIsCurrent =
    mutationRepositoryIncarnation === currentRepositoryIncarnation;
  const mutationIsPending = mutationStateIsCurrent && runScan.isPending;
  const mutationIsSuccess = mutationStateIsCurrent && runScan.isSuccess;
  const mutationIsError = mutationStateIsCurrent && runScan.isError;
  const lastScanAt =
    repo.status?.lastScanAt ?? repo.status?.lastSuccessfulScanAt;
  const latestObservedRunID = latestRunBinding.run?.id?.trim();
  const currentMutationQueryObservations = {
    repositoryIncarnation: currentRepositoryIncarnation,
    repository: JSON.stringify([
      repositoryQueryBinding.state,
      repositoryQueryBinding.observedAt ?? null,
      repo.status?.lastScanID?.trim() ?? null,
    ]),
    latestRun: JSON.stringify([
      latestRunBinding.state,
      latestRunBinding.observedAt ?? null,
      latestObservedRunID ?? null,
      latestRunBinding.run?.runUID?.trim() ?? null,
    ]),
  };
  const [mutationQueryObservationBaseline, setMutationQueryObservationBaseline] =
    useState(currentMutationQueryObservations);
  const acceptedRun = mutationIsSuccess ? runScan.data : undefined;
  const acceptedRunID = acceptedRun?.id?.trim();
  const mutationAcceptedWithoutRunID = mutationIsSuccess && !acceptedRunID;
  const settledAuthoritativeRunID =
    repositoryQueryBinding.state === "resolved" &&
    latestRunBinding.state === "resolved" &&
    !!latestObservedRunID &&
    repo.status?.lastScanID?.trim() === latestObservedRunID
      ? latestObservedRunID
      : undefined;
  const authoritativeRunWasFreshlyObserved =
    mutationQueryObservationBaseline.repositoryIncarnation ===
      currentRepositoryIncarnation &&
    mutationQueryObservationBaseline.repository !==
      currentMutationQueryObservations.repository &&
    mutationQueryObservationBaseline.latestRun !==
      currentMutationQueryObservations.latestRun;
  const acceptedRunIsFresh =
    !!acceptedRunID && settledAuthoritativeRunID === acceptedRunID;
  const acceptedRunWasSuperseded =
    !!acceptedRunID &&
    !!settledAuthoritativeRunID &&
    settledAuthoritativeRunID !== acceptedRunID &&
    authoritativeRunWasFreshlyObserved;
  const acceptedRunAwaitingObservation =
    !!acceptedRunID && !acceptedRunIsFresh && !acceptedRunWasSuperseded;
  const mutationErrorFailedAt =
    mutationErrorFreshnessGate?.repositoryIncarnation ===
    currentRepositoryIncarnation
      ? mutationErrorFreshnessGate.failedAt
      : runScan.submittedAt;
  const mutationErrorHasFreshObservations =
    mutationIsError &&
    typeof mutationErrorFailedAt === "number" &&
    mutationErrorFailedAt > 0 &&
    repositoryQueryBinding.state === "resolved" &&
    latestRunBinding.state === "resolved" &&
    (repositoryQueryBinding.observedAt ?? 0) > mutationErrorFailedAt &&
    (latestRunBinding.observedAt ?? 0) > mutationErrorFailedAt;
  const mutationErrorAwaitingFreshObservations =
    mutationIsError && !mutationErrorHasFreshObservations;

  const scanMutationBinding: LatestRunBinding | undefined = mutationIsPending
    ? {
        state: "unverified",
        unavailableDetail:
          "A new scan request is still pending; cached repository and run data cannot verify freshness.",
      }
    : mutationErrorAwaitingFreshObservations
      ? {
          state: "unverified",
          unavailableDetail:
            "The scan request failed after it may have reserved a run; waiting for fresh repository and run metadata before treating cached quality as current.",
        }
      : mutationAcceptedWithoutRunID
      ? {
          state: "unverified",
          unavailableDetail:
            "The accepted scan response has no run ID; freshness cannot be verified.",
        }
      : acceptedRunAwaitingObservation
        ? {
            state: "unverified",
            unavailableDetail: `Scan ${acceptedRunID} was accepted; waiting for fresh repository and run metadata.`,
          }
        : undefined;
  const presentation = qualityPresentation(
    repo,
    repositoryQueryBinding.state !== "resolved"
      ? repositoryQueryBinding
      : (scanMutationBinding ?? latestRunBinding),
  );
  const quality = presentation.currentQuality;
  const bundleStatus = recognizedBundleStatus(quality?.bundleStatus);
  const bundleStatusDisplay = bundleStatus
    ? displayStatus(bundleStatus)
    : "unverified";

  return (
    <Card className="transition-colors hover:border-primary/50">
      <CardHeader className="flex flex-row items-start justify-between gap-3 space-y-0">
        <div className="space-y-1">
          <CardTitle className="flex items-center gap-2 text-lg">
            <Shield className="h-5 w-5 text-primary" />
            <Link
              to="/security/$repoId"
              params={{ repoId: repo.metadata.name }}
              className="hover:underline"
            >
              {repo.spec.repository || repo.metadata.name}
            </Link>
          </CardTitle>
          <p className="text-sm text-muted-foreground">
            {repo.spec.owner}/{repo.spec.repository} ·{" "}
            {repo.spec.branch || "main"}
          </p>
        </div>
        <div className="flex flex-wrap justify-end gap-2">
          <Badge variant={presentation.variant}>{presentation.label}</Badge>
          <Badge
            variant={repo.status?.phase === "Ready" ? "default" : "secondary"}
          >
            {repo.status?.phase || "Pending"}
          </Badge>
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="text-sm text-muted-foreground">{repo.spec.repoURL}</div>
        <div className="grid gap-2 text-sm md:grid-cols-2">
          <div>
            Open findings:{" "}
            <span className="font-medium text-foreground">
              {repo.status?.findingCounts?.total ?? 0}
            </span>
          </div>
          <div>
            Last scan:{" "}
            <span className="font-medium text-foreground">
              {timeAgo(lastScanAt)}
            </span>
          </div>
        </div>
        <div className="flex flex-wrap gap-2 text-xs">
          <Badge variant="destructive">
            {repo.status?.findingCounts?.critical ?? 0} critical
          </Badge>
          <Badge variant="destructive">
            {repo.status?.findingCounts?.high ?? 0} high
          </Badge>
          <Badge variant="secondary">
            {repo.status?.findingCounts?.medium ?? 0} medium
          </Badge>
          <Badge variant="outline">
            {repo.status?.findingCounts?.low ?? 0} low
          </Badge>
        </div>
        <div className="space-y-2 rounded-md border bg-muted/30 p-3">
          {quality && (
            <div className="flex flex-wrap gap-2 text-xs">
              <Badge
                aria-label={`inventory: ${displayStatus(quality.inventoryCoverageStatus)}`}
                variant={statusVariant(
                  quality.inventoryCoverageStatus,
                  ["complete"],
                  ["partial", "failed"],
                )}
              >
                inventory: {displayStatus(quality.inventoryCoverageStatus)}
              </Badge>
              <Badge
                aria-label={`candidates: ${displayStatus(quality.candidateCoverageStatus)}`}
                variant={statusVariant(
                  quality.candidateCoverageStatus,
                  ["complete"],
                  ["partial", "failed"],
                )}
              >
                candidates: {displayStatus(quality.candidateCoverageStatus)}
              </Badge>
              <Badge
                aria-label={`coverage: ${displayStatus(quality.coverageStatus)}`}
                variant={statusVariant(
                  quality.coverageStatus,
                  ["complete"],
                  ["partial", "failed"],
                )}
              >
                coverage: {displayStatus(quality.coverageStatus)}
              </Badge>
              <Badge
                aria-label={`target: ${displayStatus(quality.targetVerification)}`}
                variant={statusVariant(
                  quality.targetVerification,
                  ["verified"],
                  ["mismatch"],
                )}
              >
                target: {displayStatus(quality.targetVerification)}
              </Badge>
              <Badge
                aria-label={`authorization: ${displayStatus(quality.authorizationStatus)}`}
                variant={statusVariant(
                  quality.authorizationStatus,
                  ["verified", "admitted"],
                  ["revoked", "expired"],
                )}
              >
                authorization: {displayStatus(quality.authorizationStatus)}
              </Badge>
              <Badge
                aria-label={`validation: ${displayStatus(quality.validationScope)}/${displayStatus(quality.validationExecution)}`}
                variant={statusVariant(
                  quality.validationExecution,
                  ["complete"],
                  ["partial", "failed"],
                )}
              >
                validation: {displayStatus(quality.validationScope)}/
                {displayStatus(quality.validationExecution)}
              </Badge>
              <Badge
                aria-label={`attack path: ${displayStatus(quality.attackPathExecution)}`}
                variant={statusVariant(
                  quality.attackPathExecution,
                  quality.validationScope === "off"
                    ? ["complete", "deferred"]
                    : ["complete"],
                  ["partial", "failed"],
                )}
              >
                attack path: {displayStatus(quality.attackPathExecution)}
              </Badge>
              <Badge
                aria-label={`attestation: ${displayStatus(quality.analysisAttestationLevel)}`}
                variant={statusVariant(
                  quality.analysisAttestationLevel,
                  ["tool-observed", "brokered"],
                  ["unverified"],
                )}
              >
                attestation: {displayStatus(quality.analysisAttestationLevel)}
              </Badge>
              <Badge
                aria-label={`isolation: ${displayStatus(quality.isolationStatus)}`}
                variant={statusVariant(
                  quality.isolationStatus,
                  ["hardened"],
                  ["fallback", "failed"],
                )}
              >
                isolation: {displayStatus(quality.isolationStatus)}
              </Badge>
              <Badge
                aria-label={`bundle: ${bundleStatusDisplay}`}
                variant={statusVariant(
                  bundleStatus,
                  ["sealed"],
                  failedBundleStatuses,
                )}
              >
                bundle: {bundleStatusDisplay}
              </Badge>
            </div>
          )}
          <p
            className="text-xs text-muted-foreground"
            aria-label="Quality details"
          >
            {conciseDetails(presentation.details)}
          </p>
        </div>
        <div className="flex items-center justify-between">
          <Link to="/security/$repoId" params={{ repoId: repo.metadata.name }}>
            <Button variant="outline">Open</Button>
          </Link>
          <Button
            variant="secondary"
            onClick={() => {
              setMutationRepositoryIncarnation(currentRepositoryIncarnation);
              setMutationQueryObservationBaseline(
                currentMutationQueryObservations,
              );
              setMutationErrorFreshnessGate(undefined);
              runScan.mutate(undefined, {
                onError: () => {
                  setMutationErrorFreshnessGate({
                    repositoryIncarnation: currentRepositoryIncarnation,
                    failedAt: Date.now(),
                  });
                },
                onSuccess: () => {
                  setMutationErrorFreshnessGate((gate) =>
                    gate?.repositoryIncarnation ===
                    currentRepositoryIncarnation
                      ? undefined
                      : gate,
                  );
                },
              });
            }}
            disabled={
              mutationIsPending ||
              mutationAcceptedWithoutRunID ||
              acceptedRunAwaitingObservation
            }
          >
            <RefreshCcw className="mr-2 h-4 w-4" />
            Scan Now
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
