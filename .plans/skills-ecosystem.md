# Orka Skills/Plugin Ecosystem — Design Plan

> **Status**: Proposed  
> **Date**: 2026-02-16  
> **Author**: AI-assisted design  

## TL;DR

Adopt the [Agent Skills](https://agentskills.io/) open standard (`SKILL.md` format), create a first-class Skill CRD to represent skills as Kubernetes resources, and integrate with Context7's registry (12K+ skills) for marketplace/discovery. This puts Orka into the same ecosystem as Claude Code, Cursor, VS Code, Copilot, Gemini CLI, and Codex.

---

## 1. Competitive Analysis

### Existing Skill Marketplaces

| Marketplace | Skills Count | Format | Distribution | How it works |
|---|---|---|---|---|
| **Context7 Skills Registry** (context7.com/skills) | **12,034** | Agent Skills (`SKILL.md`) | GitHub repos indexed | Context7 crawls GitHub repos for `SKILL.md` files, indexes them, provides search API + CLI |
| **Anthropic Skills** (github.com/anthropics/skills) | ~30 official | Agent Skills (`SKILL.md`) | GitHub repo, Claude Code plugin marketplace | Canonical reference skills; installable via `/plugin marketplace add` |
| **Vercel Agent Skills** (github.com/vercel-labs/agent-skills) | ~10 | Agent Skills (`SKILL.md`) | GitHub repo, indexed by Context7 | React/Next.js best practices skills |
| **ArtifactHub** | 0 skills | N/A | Helm charts, OCI | Has Kagent agents kind but zero published AI skills |

### The Agent Skills Open Standard (agentskills.io)

This is the industry-wide standard that all major players have adopted:

- **Created by Anthropic**, released as an open standard
- **Adopted by**: Cursor, VS Code, GitHub Copilot, Gemini CLI, OpenAI Codex, OpenCode, Factory, Spring AI, and more
- **Format**: `SKILL.md` files with YAML frontmatter in a directory structure
- **Reference library**: Available at github.com/agentskills/agentskills for validation
- **Universal install path**: `.agents/skills/` (cross-tool), `.claude/skills/` (Claude), `.cursor/skills/` (Cursor)

### Context7 Skills Registry

Context7 (by Upstash, 45.9K GitHub stars) operates the primary skills marketplace:

| Aspect | Detail |
|---|---|
| **Format** | Agent Skills open standard — `SKILL.md` with YAML frontmatter (`name`, `description`) |
| **Source** | Skills indexed from GitHub repos (e.g., `/anthropics/skills`, `/vercel-labs/agent-skills`) |
| **Install** | `npx ctx7 skills install /owner/repo skill-name` → copies `SKILL.md` into client's skill dir |
| **Search** | `npx ctx7 skills search <query>` — CLI searches the registry |
| **Trust** | Trust scores (0-10) based on stars, activity, age. Prompt injection scanning. |
| **Clients** | Claude Code, Cursor, GitHub Copilot, Gemini CLI, OpenCode, Codex — via universal target (`.agents/skills/`) |
| **AI generation** | `npx ctx7 skills generate` — AI-powered skill creation wizard |

### Broader Ecosystem Comparison

| Ecosystem | Authoring Format | Distribution | Versioning | Discovery | Trust Model |
|---|---|---|---|---|---|
| **Claude Code Skills** | `SKILL.md` + YAML frontmatter in directory | File-system: `~/.claude/skills/`, `.claude/skills/`, plugins | None (file-based) | Auto-discovery by description matching; `/` command menu | Scope-based: enterprise > personal > project |
| **Cline Skills** | `SKILL.md` in `.cline/skills/<name>/` | Git repos, checked into VCS | Git-level (commit hash) | Manual file browsing | Trust-the-repo model |
| **Helm Charts** | `Chart.yaml` + `values.yaml` + templates | OCI artifacts or Helm repos | SemVer in `Chart.yaml` | ArtifactHub, `helm search` CLI | Provenance signatures |
| **Backstage Plugins** | npm packages with plugin API | npm registry | SemVer (npm) | Plugin Directory | npm audit, community reviews |
| **Terraform Registry** | Go binary (providers), HCL modules | Terraform Registry (central) | SemVer, immutable releases | registry.terraform.io | Official/Partner/Community tiers, GPG signing |

### Key Takeaways

- **No one uses OCI for skills** — they're just markdown in Git repos. OCI adds no value for text content.
- **Agent Skills standard** is the clear winner — adopted by all major AI coding tools.
- **Context7** is the de facto marketplace — 12K+ skills, search, trust scoring, prompt injection scanning.
- ArtifactHub has no skill category and zero adoption for AI skills.

---

## 2. Current State of Skills in Orka

Skills today are **ConfigMaps with markdown content** — and they're not even injected into prompts. The implementation is a stub:

| Aspect | Current State | Implication |
|---|---|---|
| **Skill types** | `SkillReference` + `ConfigMapReference` defined in `task_types.go` | New Skill CRD replaces these; migration path needed |
| **Validation** | `validateSkills()` only checks ConfigMap existence | Skill CRD should validate content, version, dependencies |
| **Prompt injection** | **Not implemented** — skills are never read or injected | Must be added to job builder + AI worker + chat |
| **Metrics** | `orka_skills_loaded_total` exists but is unused | Wire it up when implementing injection |
| **Labels** | `orka.ai/skill: "true"` convention on ConfigMaps | Carry forward or embed in CRD |
| **Default key** | `skill.md` in ConfigMap | Skill CRD makes this a first-class `spec.content` field |
| **REST API** | No skill endpoints | Add CRUD endpoints |
| **CLI** | No skill commands | Add `orka skill list/get/create/delete` |
| **UI** | No skill pages | Add routes + components |
| **Chat** | Mentions skills but doesn't load them | Must integrate skill reading |

---

## 3. Proposed Skill CRD Schema

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Skill
metadata:
  name: code-review
  namespace: default
  labels:
    orka.ai/category: "development"
spec:
  # --- Agent Skills Standard Fields ---
  displayName: "Code Review Expert"
  description: "Systematic code review methodology with security, performance, and maintainability analysis"
  version: "1.2.0"
  author: "orka-community"
  tags: ["code-review", "security", "best-practices"]

  # --- Content ---
  content:
    # Option A: inline markdown (Agent Skills SKILL.md content)
    inline: |
      When reviewing code, follow this methodology:
      1. **Security**: Check for injection, auth bypass, secrets in code
      2. **Performance**: Identify N+1 queries, unnecessary allocations
      3. **Maintainability**: Check naming, structure, test coverage
      ...
    # Option B: ConfigMap reference (backward compat)
    configMapRef:
      name: skill-code-review
      key: skill.md

  # --- Source Tracking ---
  source:
    # Where this skill came from (for updates)
    github: "/anthropics/skills"    # GitHub repo path
    skillName: "code-review"        # Skill name within the repo
    context7: true                  # Whether indexed by Context7

status:
  phase: Ready  # Ready, Error
  contentHash: "sha256:def456..."
  conditions:
    - type: Ready
      status: "True"
      reason: ContentValid
      message: "Skill content validated successfully"
  observedGeneration: 3
```

### Design Decisions

| Question | Decision | Rationale |
|---|---|---|
| Namespaced or cluster-scoped? | **Namespaced** | Matches all other Orka CRDs; multi-tenancy friendly |
| Content format? | **Agent Skills standard** (`SKILL.md` markdown) | Industry standard adopted by Claude Code, Cursor, Copilot, Gemini, Codex |
| Inline or reference? | **Both** — `spec.content.inline` or `spec.content.configMapRef` | Inline for simplicity; ConfigMapRef for backward compat |
| Include tool definitions? | **No** — skills reference tools, don't define them | Keep CRD concerns separated; Tool CRD handles this |
| Parameterization? | **Defer** — Agent Skills standard doesn't define params | Can add `spec.parameters` in v2 if demand emerges |
| Skill composition? | **No** — keep it simple | Skills are independent markdown documents |
| SkillSet bundles? | **Defer** — label selectors can group skills | Not needed for MVP |

### Updated SkillReference Struct

```go
type SkillReference struct {
    // Name references a Skill CR by name (new, preferred)
    Name string `json:"name,omitempty"`
    // ConfigMapRef references a ConfigMap (deprecated, backward compat)
    ConfigMapRef *ConfigMapReference `json:"configMapRef,omitempty"`
}
```

---

## 4. Distribution & Registry Model

### Primary: Context7 as Marketplace

No custom registry to build. Orka integrates with Context7's existing registry:

| Aspect | Design |
|---|---|
| **Search** | `orka skill search <query>` calls Context7 API |
| **Install** | `orka skill install /owner/repo skill-name` fetches SKILL.md from GitHub, creates Skill CR in-cluster |
| **Browse** | Dashboard skills page queries Context7 for browsable/searchable catalog |
| **Publish** | Publish skills by pushing to any GitHub repo. Context7 auto-indexes SKILL.md files |
| **Trust** | Context7 trust scores (0-10). Prompt injection scanning handled by Context7 |
| **Versioning** | Git-level versioning. `spec.source.github` tracks origin for update detection |

### Secondary: Direct Creation

Users can also create skills without Context7:
- `orka skill create -f skill.yaml` — apply a Skill CR directly
- `kubectl apply -f skill.yaml` — standard K8s workflow
- Dashboard "Create Skill" page with inline markdown editor
- GitOps: Skill CRDs committed to Git, applied via Kustomize/ArgoCD/Flux

### Offline/Air-gapped

Context7 integration is optional — only for discovery. The Skill CRD works fully offline. Users can:
- `orka skill import <path/to/SKILL.md>` — create Skill CR from local file
- Copy SKILL.md content into `spec.content.inline`

### Why Not OCI?

No one in the AI skills ecosystem uses OCI. Skills are small markdown files (< 100KB). Git provides versioning, diffing, PRs for review. Context7 provides discovery on top of Git. OCI adds infrastructure complexity without value.

---

## 5. Discovery & Marketplace UX

### API Endpoints

| Method | Path | Description |
|---|---|---|
| `POST` | `/api/v1/skills` | Create a Skill CR |
| `GET` | `/api/v1/skills` | List skills (supports `?tag=`, `?search=`) |
| `GET` | `/api/v1/skills/:name` | Get skill detail |
| `PUT` | `/api/v1/skills/:name` | Update skill |
| `DELETE` | `/api/v1/skills/:name` | Delete skill |
| `GET` | `/api/v1/skills/:name/content` | Get raw skill content |
| `GET` | `/api/v1/skills/registry/search` | Search Context7 registry (proxy) |

### CLI Commands

| Command | Description |
|---|---|
| `orka skill list` | List skills in current namespace |
| `orka skill get <name>` | Show skill details |
| `orka skill create -f skill.yaml` | Create skill from manifest |
| `orka skill delete <name>` | Delete skill |
| `orka skill search <query>` | Search Context7 registry |
| `orka skill install /owner/repo skill-name` | Install from Context7/GitHub |
| `orka skill import <path>` | Create from local SKILL.md file |

### Dashboard UI

| Route | Page | Description |
|---|---|---|
| `/skills` | Skills list | Card grid with name, description, version, tags, status. Search bar. "Browse Registry" tab queries Context7. |
| `/skills/:skillName` | Skill detail | Metadata, rendered markdown preview, which agents use it, source info |
| `/skills/new` | Create skill | Form with inline markdown editor, tag selector |

---

## 6. In-Agent Skill Experience (Runtime)

### Critical Gap: Skills Are Never Injected Today

The runtime integration must be built end-to-end:

| Component | Change |
|---|---|
| **Job builder** (`internal/controller/job_builder.go`) | Read Skill CRs, pass content via env var or volume mount |
| **AI worker** (`workers/ai/main.go`) | Read skill content, prepend to system prompt |
| **Chat** (`internal/api/chat_system_prompt.go`) | Read Skill CRs, include in dynamic system prompt |
| **Metrics** (`internal/metrics/metrics.go`) | Wire existing `orka_skills_loaded_total` counter |

### Prompt Injection Design

1. Job builder reads all Skill CRs referenced by the agent/task
2. Content concatenated in declaration order (agent-level first, then task-level)
3. Injected after base system prompt, before tool descriptions
4. Passed to worker via `ORKA_AI_SKILLS` env var (or mounted volume for large skills)
5. AI worker prepends skill content to system prompt

### Content Size Guard

- Warn if total skill content exceeds 10KB (~2500 tokens)
- Agent Skills spec recommends SKILL.md under 500 lines
- Skills should not consume more than ~25% of model context window

---

## 7. Phased Implementation Plan

### Phase 1: Skill CRD & Runtime Injection (~2 weeks)

**Goal**: Replace ConfigMap stubs with first-class CRD. Wire skills into agent prompts. Basic CRUD.

| Step | Task | Files | Depends On |
|---|---|---|---|
| 1 | Define Skill CRD types | `api/v1alpha1/skill_types.go` (new) | — |
| 2 | Generate CRD manifests | `make manifests generate` | Step 1 |
| 3 | Create Skill controller | `internal/controller/skill_controller.go` (new) | Step 2 |
| 4 | Update SkillReference | `api/v1alpha1/task_types.go`, `agent_types.go` | Step 1 |
| 5 | Update agent controller validation | `internal/controller/agent_controller.go` | Step 4 |
| 6 | Wire prompt injection (job builder) | `internal/controller/job_builder.go` | Steps 3, 4 |
| 7 | Wire into AI worker | `workers/ai/main.go` | Step 6 |
| 8 | Wire into chat system prompt | `internal/api/chat_system_prompt.go` | Step 3 |
| 9 | Add REST API endpoints | `internal/api/handlers.go`, `server.go` | Step 3 |
| 10 | Add CLI commands | `cmd/cli/skill.go` (new) | Step 9 |
| 11 | Add UI pages | `ui/src/routes/skills/` (new) | Step 9 |
| 12 | Migrate samples | `config/samples/`, `examples/` | Step 1 |
| 13 | Tests | Throughout | Throughout |

**Verification checklist:**
- [ ] `make manifests generate` succeeds
- [ ] `make lint-fix && make test` passes
- [ ] Skill CR creates with `Ready` status
- [ ] Agent referencing a Skill CR has skill content in its system prompt
- [ ] Old ConfigMap-based skills still work (backward compat)
- [ ] `orka skill list` returns skills
- [ ] UI `/skills` page renders skill cards
- [ ] `orka_skills_loaded_total` metric increments when skills are loaded

### Phase 2: Context7 Integration (~1–2 weeks)

**Goal**: Connect Orka to Context7's registry for search/install of 12K+ skills.

| Step | Task | Files |
|---|---|---|
| 1 | Context7 search integration | `internal/registry/context7.go` (new), `cmd/cli/skill.go` |
| 2 | Context7 install (GitHub fetch + Skill CR creation) | `internal/registry/context7.go`, `cmd/cli/skill.go` |
| 3 | Dashboard "Browse Registry" tab | `ui/src/routes/skills/index.tsx` |
| 4 | Skill suggest based on agent config | `cmd/cli/skill.go` |
| 5 | Update detection (upstream changed) | `internal/controller/skill_controller.go` |

**Verification checklist:**
- [ ] `orka skill search react` returns skills from Context7
- [ ] `orka skill install /anthropics/skills pdf` creates a working Skill CR
- [ ] Dashboard shows browsable Context7 skills
- [ ] Agent using an installed Context7 skill gets its content in the prompt

### Phase 3: Ecosystem & Community (~ongoing)

**Goal**: Publish Orka-specific skills, contribute to the ecosystem.

| Step | Task |
|---|---|
| 1 | Create `github.com/orka-skills` org with curated skills |
| 2 | `orka skill init` scaffolds SKILL.md template |
| 3 | `orka skill validate` checks against Agent Skills spec |
| 4 | Wire skill analytics (load counts, agent references) |
| 5 | Optionally list on ArtifactHub for additional visibility |

---

## 8. Risk Analysis

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| **Skills bloat system prompts** → token overflow | High | High | Content size validation; warn above 10KB; Agent Skills spec recommends < 500 lines |
| **Backward compat break** — ConfigMap skills stop working | Medium | High | `SkillReference` supports both `Name` and `ConfigMapRef`; tests for both |
| **Context7 API instability** — breaking changes | Medium | Medium | Abstract behind `SkillRegistry` interface; fallback to direct GitHub fetch |
| **Context7 unavailable** — air-gapped clusters | Low | Medium | Context7 is optional; Skill CRD works fully offline; `orka skill import` for manual use |
| **Security: malicious skill content** — prompt injection | Medium | High | Context7 scans for prompt injection; skills are markdown (not executable); namespace isolation |
| **Adoption: skills not useful** — low engagement | Medium | High | Ship 10+ high-quality curated skills; show concrete agent improvements |

---

## 9. Success Metrics

| Metric | Phase 1 Target | Phase 3 Target |
|---|---|---|
| Skills created per cluster | 5+ (samples + user-created) | 20+ |
| Agents using skills | >50% of agents have ≥1 skill | >80% |
| `orka_skills_loaded_total` counter | Monotonically increasing | Track per-skill popularity |
| Community-published skills | N/A | 50+ on Context7/GitHub |
| CLI skill command usage | Measurable | Top-3 CLI command group |
| Dashboard skills page visits | Measurable | Comparable to tools page |

---

## 10. Key Files Reference

| File | Purpose |
|---|---|
| `api/v1alpha1/task_types.go` (~line 297) | Current `SkillReference`, `ConfigMapReference` — to modify |
| `api/v1alpha1/agent_types.go` | `AgentSpec.Skills` — to update |
| `api/v1alpha1/tool_types.go` | Tool CRD as pattern (status, conditions, controller) |
| `api/v1alpha1/groupversion_info.go` | CRD registration pattern |
| `internal/controller/agent_controller.go` (~line 165) | `validateSkills()` — to update |
| `internal/controller/tool_controller.go` | Controller pattern to follow |
| `internal/controller/job_builder.go` | **Critical**: missing skill prompt injection |
| `workers/ai/main.go` | AI worker: add skill content to system prompt |
| `internal/api/chat_system_prompt.go` | Chat: add skill content |
| `internal/api/handlers.go` | Add skill CRUD handlers |
| `internal/api/server.go` | Register skill routes |
| `cmd/cli/agent.go` | CLI pattern for skill commands |
| `internal/metrics/metrics.go` | Existing `orka_skills_loaded_total` |
| `examples/complex-workflow/skills.yaml` | Existing ConfigMap skills to migrate |

---

## 11. Open Questions & Decisions

| Question | Decision | Rationale |
|---|---|---|
| Namespaced or cluster-scoped? | **Namespaced** | Matches all other Orka CRDs; multi-tenancy |
| Content format? | **Agent Skills standard** (SKILL.md) | Industry standard — Claude Code, Cursor, Copilot, Gemini, Codex |
| Marketplace? | **Context7** (existing, 12K+ skills) | No custom registry to build or maintain |
| Distribution? | **Git repos** (not OCI) | Industry consensus; skills are markdown, not binaries |
| Backward compat? | **Both Name + ConfigMapRef** | Old ConfigMap skills keep working |
| Parameterization? | **Defer** | Agent Skills standard doesn't define params yet |
| ClusterSkill CRD? | **Defer** | Can add cluster-scoped variant later if needed |

### Further Considerations

1. **Context7 API abstraction** — Abstract behind a `SkillRegistry` interface so alternative registries can be swapped in.
2. **Offline support** — `orka skill import <SKILL.md>` for air-gapped clusters.
3. **Content size limits** — 10KB soft limit with validation warning. Large skills degrade LLM performance.
4. **Prompt ordering** — Agent-level skills injected first, then task-level. Declaration order preserved.
