# Orka Artifacts Feature Plan
# Goal: Enable "create a powerpoint" → download locally

## Problem Statement

Orka workers run in ephemeral pods with emptyDir volumes. When a pod terminates,
all generated files are lost. There is no file_write tool, no artifact storage,
and no download mechanism. Users cannot retrieve binary files (PowerPoint, PDF,
images, etc.) produced by AI agents.

## Architecture Decision

**Hybrid approach**: Controller-mediated artifact storage + optional PV mount via pv-mounter.

- Controller API handles artifact upload/download (works everywhere, no extra infra)
- PVC-backed workspace is opt-in for large files or live browsing via pv-mounter
- Both paths coexist — small artifacts go through the API, large workspaces use PVCs

---

## Phase 1: file_write Tool + Artifact Upload (Core Plumbing)
Status: not-started

### 1.1 Add file_write tool
- File: internal/tools/file_write.go
- New tool: `file_write` — writes content to /tmp/artifacts/<filename>
- Parameters: path (string, required), content (string, required), encoding (string, optional: "text"|"base64")
- Path validation: must resolve under /tmp/artifacts/ (no traversal)
- Max file size: 10MB per file
- Auto-creates /tmp/artifacts/ directory
- Register in registry.go alongside file_read

### 1.2 Add file_write tool tests
- File: internal/tools/file_write_test.go
- Test: write text file, write base64-encoded binary, path traversal rejection,
  size limit enforcement, overwrite behavior

### 1.3 Worker artifact upload on completion
- File: workers/common/artifacts.go
- New function: UploadArtifacts() — scans /tmp/artifacts/, uploads each file
  to POST /internal/v1/artifacts/{namespace}/{taskName}/{filename}
- Content-Type detection via http.DetectContentType or extension mapping
- Called from AI worker main.go after SubmitResult()
- Retry logic similar to SubmitResult (5 retries, exponential backoff)
- Max total artifact size: 50MB (configurable via ORKA_MAX_ARTIFACT_SIZE)

---

## Phase 2: Artifact Storage + API Endpoints
Status: not-started

### 2.1 Artifact store interface
- File: internal/store/artifact_store.go
- Interface: ArtifactStore
  - SaveArtifact(ctx, namespace, taskName, filename, contentType string, data []byte) error
  - GetArtifact(ctx, namespace, taskName, filename string) (data []byte, contentType string, err error)
  - ListArtifacts(ctx, namespace, taskName string) ([]ArtifactMetadata, error)
  - DeleteArtifacts(ctx, namespace, taskName string) error
- ArtifactMetadata: {Filename, ContentType, Size, CreatedAt}

### 2.2 SQLite artifact store implementation
- File: internal/store/sqlite/artifact_store.go
- New table: artifacts (namespace TEXT, task_name TEXT, filename TEXT, content_type TEXT,
  size INTEGER, data BLOB, created_at TIMESTAMP, PRIMARY KEY (namespace, task_name, filename))
- Migration: add table in schema.go or migrations
- Individual file size limit: 10MB (enforced at store level)

### 2.3 Internal API: artifact upload endpoint
- File: internal/api/internal_handlers.go (extend)
- POST /internal/v1/artifacts/{namespace}/{taskName}/{filename}
  - Body: raw binary (application/octet-stream)
  - Headers: Content-Type (for metadata)
  - Auth: SA token (same as result submission)
  - Response: 201 Created or 409 if exists (with overwrite query param)
  - Max body: 10MB

### 2.4 Public API: artifact list + download endpoints
- File: internal/api/handlers.go (extend)
- GET /api/v1/tasks/{id}/artifacts
  - Response: {"artifacts": [{"filename": "...", "contentType": "...", "size": 1234}]}
  - Auth: bearer token
- GET /api/v1/tasks/{id}/artifacts/{filename}
  - Response: raw binary with Content-Type and Content-Disposition headers
  - Auth: bearer token
  - Supports Range header for partial downloads (stretch goal)

### 2.5 Artifact cleanup
- When a task is deleted, its artifacts are also deleted (via store.DeleteArtifacts)
- Hook into existing task cleanup/GC logic in controller

---

## Phase 3: CLI Support
Status: not-started

### 3.1 CLI client methods
- File: internal/cli/client/client.go (extend)
- ListArtifacts(taskName string) ([]ArtifactMetadata, error)
- DownloadArtifact(taskName, filename, destPath string) error

### 3.2 CLI commands
- File: cmd/cli/task.go or cmd/cli/artifacts.go (new subcommand)
- `orka task artifacts <name>` — list artifacts with filename, type, size
- `orka task download <name> [filename] [--output <path>]`
  - If filename omitted and only one artifact, download it
  - If filename omitted and multiple, list them and prompt (or download all with --all)
  - Default output: current directory with original filename
  - --output: specify destination path or directory

---

## Phase 4: PVC-backed Workspace (Optional, for Large Files / Live Mount)
Status: not-started

### 4.1 Task spec: artifact volume configuration
- File: api/v1alpha1/task_types.go (extend TaskSpec)
- New field: ArtifactVolume *ArtifactVolumeSpec
  ```go
  type ArtifactVolumeSpec struct {
      // PersistentVolumeClaim creates a PVC for the artifact directory
      PVC *PVCArtifactSpec `json:"pvc,omitempty"`
      // RetainAfterCompletion keeps the PVC after task completion (default: true)
      RetainAfterCompletion *bool `json:"retainAfterCompletion,omitempty"`
  }
  type PVCArtifactSpec struct {
      StorageClassName *string `json:"storageClassName,omitempty"`
      Size             string  `json:"size"` // e.g. "1Gi"
      AccessModes      []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`
  }
  ```
- Run: make manifests generate

### 4.2 Controller: PVC lifecycle management
- File: internal/controller/task_controller.go (extend)
- Before Job creation: if task.Spec.ArtifactVolume.PVC is set, create a PVC
  named `{taskName}-artifacts` with specified storage class and size
- Owner reference: set to Task (so PVC is GC'd when Task is deleted, unless RetainAfterCompletion)
- If RetainAfterCompletion: don't set owner reference, add label instead

### 4.3 Job builder: mount PVC instead of emptyDir
- File: internal/controller/job_builder.go (extend)
- When task.Spec.ArtifactVolume.PVC is set:
  - Add PVC volume source instead of emptyDir for /workspace (or /artifacts)
  - Set access mode from spec (default RWO)

### 4.4 CLI: mount command (pv-mounter integration)
- File: cmd/cli/mount.go (new)
- `orka task mount <name> <local-path>`
  - Resolves task → finds PVC name from task status or labels
  - Shells out to `kubectl pv-mounter mount <namespace> <pvcName> <localPath>`
  - Requires: kubectl + pv-mounter plugin + SSHFS installed
- `orka task unmount <name> <local-path>`
  - Shells out to `kubectl pv-mounter clean <namespace> <pvcName> <localPath>`
- Prerequisites check: verify pv-mounter is installed, print install instructions if not

---

## Phase 5: UI Support
Status: not-started

### 5.1 Artifact list in task detail view
- File: ui/src/ (task detail component)
- Show artifacts section when artifacts exist
- Table: filename, type, size, download button
- Download button triggers browser download via GET /api/v1/tasks/{id}/artifacts/{filename}

### 5.2 Artifact preview (stretch)
- Inline preview for text, images, markdown
- "Open in new tab" for other types

---

## Phase 6: Worker Image with Common Packages
Status: not-started

### 6.1 Enhanced AI worker image
- File: Dockerfile (or new Dockerfile.ai-worker-full)
- Pre-install common Python packages for file generation:
  - python-pptx (PowerPoint)
  - openpyxl (Excel)
  - reportlab / fpdf2 (PDF)
  - Pillow (images)
  - matplotlib (charts)
- Tag as ghcr.io/sozercan/orka/ai-worker-full:latest
- Agent CRD can specify which worker image to use

### 6.2 pip install in code_exec (interim)
- Already works today (writes to /tmp)
- Document as a pattern: agent uses code_exec with `pip install python-pptx && python script.py`
- Slow but functional without image changes

---

## End-to-End Flow

```
User:   orka run --agent my-agent "Create a 5-slide PowerPoint about Kubernetes"

AI Agent:
  1. code_exec: pip install python-pptx (if not in image)
  2. code_exec: python3 -c "from pptx import ...; create slides; prs.save('/tmp/artifacts/k8s-intro.pptx')"
     OR
  2. file_write: path="k8s-intro.pptx", content=<base64>, encoding="base64"
  3. Returns summary: "Created k8s-intro.pptx with 5 slides covering..."

Worker:
  4. SubmitResult(summary)
  5. UploadArtifacts() → POST /internal/v1/artifacts/default/my-task/k8s-intro.pptx

User (CLI):
  6. orka task artifacts my-task
     → k8s-intro.pptx  application/vnd.openxmlformats-officedocument.presentationml.presentation  2.1MB
  7. orka task download my-task k8s-intro.pptx
     → Saved to ./k8s-intro.pptx

User (PVC mount, optional):
  6. orka task mount my-task ~/orka-files
     → Mounted at ~/orka-files (via pv-mounter + SSHFS)
  7. open ~/orka-files/k8s-intro.pptx
     → Opens in PowerPoint/Keynote directly

User (UI):
  6. Browse to task detail → Artifacts tab → Click "Download"
```

---

## Implementation Order

Priority: Phase 1 → Phase 2 → Phase 3 → Phase 6 → Phase 4 → Phase 5

Rationale:
- Phases 1-3 deliver the core flow: write files, store them, download them
- Phase 6 makes common file types work without pip install each time
- Phase 4 adds the pv-mounter power-user flow for large/interactive workloads
- Phase 5 is UI polish

## Open Questions

1. Should artifacts count against a per-task storage quota? (Prevents abuse in multi-tenant)
2. Should artifacts have TTL independent of task TTL?
3. Should the file_write tool support streaming/chunked writes for very large files?
4. Should we support S3/MinIO as an alternative artifact backend for large-scale deployments?
5. Should PVC storage class be configurable at the Agent level (not just Task)?
