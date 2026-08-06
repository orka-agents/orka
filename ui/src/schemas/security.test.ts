import { describe, expect, it } from 'vitest'
import {
  findingAssessmentSchema,
  findingDecisionSchema,
  findingOccurrenceSchema,
  repositoryScanSpecSchema,
  scanRunSchema,
  threatModelSchema,
} from './security'

describe('repositoryScanSpecSchema', () => {
  it('preserves a pinned repository ref', () => {
    const spec = {
      repoURL: 'https://github.com/example/repository.git',
      branch: 'main',
      ref: 'refs/tags/v1.2.3',
      analysisAgentRef: { name: 'security-agent' },
    }

    expect(repositoryScanSpecSchema.parse(spec)).toEqual(spec)
  })
})

describe('scanRunSchema', () => {
  it('preserves API scan-run identity and quality fields', () => {
    const scanRun = {
      id: 'scan-123',
      runUID: 'run-uid-123',
      namespace: 'default',
      repositoryScan: 'example',
      repositoryScanUID: 'repository-scan-uid-123',
      repositoryScanGeneration: 7,
      taskName: 'scan-task-123',
      mode: 'incremental',
      phase: 'succeeded',
      startedAt: '2026-08-01T12:00:00Z',
      quality: {
        qualitySchemaVersion: 2,
        inventoryCoverageStatus: 'complete',
        candidateCoverageStatus: 'complete',
        coverageStatus: 'complete',
        validationScope: 'full',
        validationExecution: 'completed',
        attackPathExecution: 'completed',
        analysisAttestationLevel: 'tool-observed',
        targetVerification: 'verified',
        bundleStatus: 'sealed',
        authorizationStatus: 'verified',
        isolationStatus: 'hardened',
        reasonCodes: ['all_checks_passed'],
      },
    }

    expect(scanRunSchema.parse(scanRun)).toEqual(scanRun)
  })
})

describe('findingOccurrenceSchema', () => {
  it('preserves immutable occurrence provenance and digests', () => {
    const occurrence = {
      id: 'occurrence-123',
      namespace: 'default',
      repositoryScan: 'example',
      scanRunID: 'scan-123',
      runUID: 'run-uid-123',
      publicFindingID: 'finding-123',
      semanticFindingID: 'semantic-123',
      semanticFingerprint: 'semantic-fingerprint',
      identityQuality: 'canonical',
      identityAlgorithmVersion: 'v2',
      legacyFingerprint: 'legacy-fingerprint',
      ruleID: 'rule-123',
      identityAnchor: 'src/security.ts:handler',
      identityInstance: 'instance-123',
      targetReceiptID: 'target-receipt-123',
      targetSHA: '0123456789abcdef0123456789abcdef01234567',
      discoveryPayload: {
        title: 'Unsafe trust-boundary transition',
        evidence: [{ path: 'src/security.ts', line: 42 }],
      },
      payloadDigest: 'payload-digest',
      observationLinks: [
        {
          observationID: 'observation-123',
          relationship: 'contributor',
          ordinal: 0,
        },
      ],
      recordDigest: 'record-digest',
      createdAt: '2026-08-01T12:01:00Z',
    }

    expect(findingOccurrenceSchema.parse(occurrence)).toEqual(occurrence)
  })
})

describe('findingDecisionSchema', () => {
  it('preserves authenticated decision evidence and applicability', () => {
    const decision = {
      decisionID: 'decision-123',
      namespace: 'default',
      repositoryScan: 'example',
      publicFindingID: 'finding-123',
      scope: 'logical_finding',
      occurrenceID: 'occurrence-123',
      action: 'suppress',
      reasonCode: 'accepted_risk',
      reason: 'Mitigated by an external control',
      evidenceReceiptIDs: ['evidence-receipt-123'],
      supersedesDecisionID: 'decision-122',
      expectedDecisionVersion: 4,
      decisionVersion: 5,
      applicability: {
        targetLineage: 'main',
        scope: 'src/security/**',
        policyVersion: 'policy-v3',
        predicateDigest: 'predicate-digest',
        expiresAt: '2026-09-01T00:00:00Z',
      },
      actorSubject: 'user@example.com',
      actorIssuer: 'https://issuer.example.com',
      authenticationSource: 'oidc',
      source: 'api',
      feedbackEligible: true,
      recordDigest: 'record-digest',
      createdAt: '2026-08-01T12:02:00Z',
    }

    expect(findingDecisionSchema.parse(decision)).toEqual(decision)
  })
})

describe('findingAssessmentSchema', () => {
  it('preserves source-run bindings, receipts, evidence, and payload digests', () => {
    const assessment = {
      id: 'assessment-123',
      namespace: 'default',
      repositoryScan: 'example',
      scanRunID: 'scan-123',
      runUID: 'run-uid-123',
      occurrenceID: 'occurrence-123',
      publicFindingID: 'finding-123',
      kind: 'validation',
      stageReceiptID: 'stage-receipt-123',
      targetReceiptID: 'target-receipt-123',
      targetSHA: '0123456789abcdef0123456789abcdef01234567',
      method: 'static-analysis',
      outcome: 'confirmed',
      failureClass: 'none',
      summary: 'The reported path is reachable.',
      proofGap: 'Runtime exploitability was not exercised.',
      evidenceReceiptIDs: ['evidence-receipt-123'],
      normalizedPayload: {
        reachablePath: ['source', 'validator', 'sink'],
        confidence: 'high',
      },
      payloadDigest: 'payload-digest',
      projectionValidationStatus: 'confirmed',
      projectionEvidence: [
        {
          kind: 'artifact',
          taskName: 'validator-task',
          name: 'security-validation.json',
          label: 'Validation JSON',
        },
      ],
      recordDigest: 'record-digest',
      createdAt: '2026-08-01T12:03:00Z',
    }

    expect(findingAssessmentSchema.parse(assessment)).toEqual(assessment)
  })
})

describe('threatModelSchema', () => {
  it('preserves RepositoryScan incarnation bindings', () => {
    const model = {
      namespace: 'default',
      repositoryScan: 'example',
      repositoryScanUID: 'repository-scan-uid-123',
      repositoryScanGeneration: 7,
      version: 2,
      content: 'Threat model',
      source: 'edited',
      createdAt: '2026-08-01T12:00:00Z',
      updatedAt: '2026-08-01T12:01:00Z',
    }

    expect(threatModelSchema.parse(model)).toEqual(model)
  })
})
