# Mercan CLI — Kanban Board TUI + GitHub Projects Integration

**Issue**: [#13](https://github.com/sozercan/mercan/issues/13)

## Vision

Replace the current two-pane streaming view with a **Kagan-style
kanban board TUI** backed by **GitHub Projects v2**. The board is the
primary interface — users see tasks flowing through columns, manage
agent workloads, and review results without leaving the terminal.

### UX Target

```
mercan                                          ⎇ main  0 running
╭───────────────────────────────────────────────────────────────────╮
│ BACKLOG (3)     │ IN PROGRESS (2)  │ REVIEW (1)    │ DONE (4)    │
│─────────────────│──────────────────│───────────────│─────────────│
│ ┌─────────────┐ │ ┌──────────────┐ │ ┌───────────┐ │ ┌─────────┐ │
│ │⚡ auth-api   │ │ │▶ frontend    │ │ │✓ architect│ │ │✓ db-migr│ │
│ │ P1 • ai     │ │ │ ⠋ 23s • ai  │ │ │  8s • ai  │ │ │  12s    │ │
│ │ JWT endpts  │ │ │ login form   │ │ │ needs rev │ │ │         │ │
│ └─────────────┘ │ ├──────────────┤ │ └───────────┘ │ ├─────────┤ │
│ ┌─────────────┐ │ │▶ backend-dev │ │               │ │✓ lint   │ │
│ │⚡ tests      │ │ │ ⠋ 18s • ai  │ │               │ │  5s     │ │
│ │ P2 • ai     │ │ │ auth logic   │ │               │ └─────────┘ │
│ │ unit tests  │ │ └──────────────┘ │               │             │
│ └─────────────┘ │                  │               │             │
╰───────────────────────────────────────────────────────────────────╯
 [n]ew  [p]lan  [/]search  [.]actions  [enter]open  [space]peek
 [h][j][k][l] navigate  [tab] column  [q] quit
```

## Design Principles

1. **Board-first** — The kanban board is home. `mercan` (no subcommand)
   launches it. The old `mercan run` becomes one way to create tasks.
2. **GitHub Projects as source of truth** — Tasks sync bidirectionally
   with a GitHub Projects v2 board via GraphQL API. Mercan Task CRDs
   are the execution layer; the project board is the planning layer.
3. **Keyboard-first** — Vim-style H/J/K/L navigation, Tab to jump
   columns, fuzzy command palette (Ctrl+P), single-key actions.
4. **Cards, not lists** — Each task is a bordered card showing status
   icon, name, priority, type badge, duration, and one-line summary.
   Running cards get pulsing/animated borders.
5. **Plan mode** — Natural-language input produces structured task
   proposals the user can approve/edit/dismiss before execution.

## GitHub Projects v2 Integration

### Data Model Mapping

| GitHub Project Field  | Mercan Concept          | Notes                            |
|-----------------------|-------------------------|----------------------------------|
| Status (single-select)| Board column            | BACKLOG, IN_PROGRESS, REVIEW, DONE |
| Title                 | Task name               |                                  |
| Body / Description    | Task prompt / spec      |                                  |
| Priority (custom)     | Task priority (0-1000)  | Map to P0/P1/P2/P3               |
| Type (custom label)   | Task type               | `container`, `ai`, `agent`       |
| Agent (custom)        | Agent name              | Which agent to delegate to       |
| Assignees             | —                       | Optional, for team visibility    |
| Labels                | —                       | Tag with `mercan/`, parent-task  |
| Iteration             | —                       | Optional sprint tracking         |

### Sync Behavior

- **Board → Mercan**: Moving a card to IN_PROGRESS creates a Task CRD
  and triggers execution. Moving to BACKLOG cancels a pending task.
- **Mercan → Board**: Task phase changes (Pending→Running→Succeeded)
  update the project item's Status field. Results populate a custom
  "Result" field or a linked comment.
- **Conflict resolution**: Mercan Task CRD status is authoritative for
  execution state. Board status is authoritative for planning intent.
- **Offline / no project**: If no GitHub project is configured, the CLI
  falls back to Mercan API only (tasks listed directly from the
  cluster). Board columns are synthesized from Task phases.

### Auth

- GitHub token via `GITHUB_TOKEN`, `gh auth token`, or
  `~/.mercan/config.yaml` → `github.token` field.
- Scopes needed: `project`, `repo` (for PR tools).
- Separate from Mercan API auth (kubeconfig / `--token`).

## Todos

### Phase 1: CLI Foundation (existing — already done)

1. ~~**cli-cobra-setup**~~ — ✅ Done. Cobra root command with global
   flags (`--server`, `--token`, `--kubeconfig`, `--namespace`, `--output`).

2. ~~**api-client**~~ — ✅ Done. REST API client in `internal/cli/client/`.

3. ~~**sse-client**~~ — ✅ Done. SSE stream reader in `internal/cli/client/sse.go`.

### Phase 2: CRUD Commands (existing — already done)

4. ~~**task-commands**~~ — ✅ Done.
5. ~~**agent-commands**~~ — ✅ Done.
6. ~~**session-tool-commands**~~ — ✅ Done.

### Phase 3: GitHub Projects Client

7. **github-projects-client** — Create `internal/cli/github/` package.
   - `projects.go` — GraphQL client for GitHub Projects v2 API.
     Methods: `GetProject(owner, number)`, `ListItems(projectID)`,
     `GetStatusField(projectID)`, `UpdateItemStatus(projectID, itemID, optionID)`,
     `AddItem(projectID, contentID)`, `AddDraftItem(projectID, title, body)`,
     `DeleteItem(projectID, itemID)`.
   - `sync.go` — Bidirectional sync logic. `SyncFromProject()` pulls
     board state into local model. `PushTaskStatus(task, phase)` updates
     project item status. `CreateTaskFromItem(item)` creates a Mercan
     Task CRD from a project card.
   - `types.go` — Go types for project items, fields, field values.
   - Auth: use `GITHUB_TOKEN` env var, fall back to `gh auth token`.

8. **project-config** — Add `--project` flag (format: `owner/number`,
   e.g. `sozercan/1`) and persist in `~/.mercan/config.yaml` as
   `github.project`. Add `mercan config set-project <owner/number>`.

### Phase 4: Kanban Board TUI (Hero Feature)

9. **board-model** — New Bubbletea model in `internal/cli/tui/board.go`.
   Full-screen alt-screen kanban board with 4 columns.
   - Columns: BACKLOG, IN_PROGRESS, REVIEW, DONE.
   - Each column is a scrollable list of task cards.
   - Focus model: column-level (Tab/Shift-Tab) + card-level (J/K or ↑/↓).
   - H/L or ←/→ moves focus between columns.
   - Responsive: columns share terminal width equally; cards truncate
     gracefully on narrow terminals (<100 cols shows 2 columns with
     horizontal scroll).

10. **card-component** — `internal/cli/tui/card.go`. Each card renders:
    - **Top line**: Status icon (⏸/▶/✓/✗) + task name (bold).
    - **Middle**: Priority badge (P0 red, P1 orange, P2 yellow, P3 gray)
      + type badge (`ai`/`agent`/`container`) + duration timer.
    - **Bottom**: One-line summary/description (truncated).
    - **Border**: Rounded. Focused card gets cyan border. Running cards
      get animated/highlighted border (lipgloss adaptive color cycling
      via `TickMsg`). Succeeded = green left-border accent.
      Failed = red left-border accent.
    - Card width adapts to column width.

11. **column-component** — `internal/cli/tui/column.go`. Each column:
    - Header: column name + item count, e.g. `IN PROGRESS (3)`.
    - Scrollable card list with visible scroll indicator.
    - Drop-target highlighting when moving cards.

12. **card-actions** — Keyboard actions on focused card:
    - `Enter` — Open task detail view (full-screen overlay).
    - `Space` — Quick-peek overlay (result summary, no full switch).
    - `n` — New task (opens inline prompt for title + description).
    - `m` — Move card to next column (BACKLOG→IN_PROGRESS starts
      execution, IN_PROGRESS→REVIEW, REVIEW→DONE merges).
    - `M` — Move card to previous column (reject/requeue).
    - `x` / `Delete` — Delete/cancel task.
    - `y` — Duplicate task.
    - `c` — Copy task name to clipboard.
    - `r` — Retry failed task (re-create with same spec).
    - `s` — Stop running task.
    - `v` — View task logs (streaming in overlay).

13. **task-detail-overlay** — `internal/cli/tui/detail.go`. Full-screen
    overlay when pressing Enter on a card:
    - **Header**: Task name, phase, type, agent, priority, created time.
    - **Tabs**: Description | Logs | Result | Diff (for tasks with
      structured results).
    - Tab navigation with number keys or Tab.
    - `e` to edit description (opens `$EDITOR`).
    - `Esc` returns to board.

14. **board-poller** — `internal/cli/tui/board_poller.go`. Background
    goroutine that:
    - Polls Mercan API (`GET /api/v1/tasks`) every 2s.
    - If GitHub project configured, also polls project items and
      reconciles.
    - Uses hash-based change detection (existing pattern from `poller.go`).
    - Emits `BoardUpdateMsg` with full board state.
    - Detects phase transitions → inline toast notifications at top.

15. **header-bar** — `internal/cli/tui/header.go`. Top bar showing:
    - Mercan logo/name (left).
    - Current branch (`⎇ main`), namespace, active agent count.
    - GitHub project link if configured.

16. **status-bar** — Update `internal/cli/tui/statusbar.go`. Bottom bar:
    - Context-sensitive keybinding hints.
    - Active task count + running duration.
    - Spinner when any task is running.
    - Toast notifications (phase transitions, errors) that auto-dismiss.

### Phase 5: Plan Mode

17. **plan-command** — `mercan plan "<description>"` or press `p` on the
    board to enter plan mode.
    - Opens a text input area at the bottom of the board.
    - User types natural-language description of work.
    - Sends to `POST /api/v1/chat` with a planning-specific system
      prompt that returns structured JSON task proposals.
    - Parses response into a list of proposed tasks.

18. **plan-approval-ui** — `internal/cli/tui/plan.go`. After plan
    generation:
    - Shows proposed tasks in a vertical list with:
      `[a]pprove  [e]dit  [d]dismiss` per task.
    - `A` approves all. `D` dismisses all.
    - Approved tasks are created as Task CRDs (and project items if
      configured).
    - Edited tasks open inline editor before creation.

### Phase 6: Run Mode Integration

19. **run-as-board-action** — `mercan run --agent <name> "<prompt>"`
    creates a task and switches to the board view with that task
    focused. The board shows live progress as the task executes.
    Non-TTY mode remains plain-text streaming (unchanged).

20. **coordinator-drawer** — When a coordinator task is focused/opened,
    show the SSE stream output in the detail overlay's Logs tab.
    Child agent tasks appear as separate cards on the board (under
    IN_PROGRESS) with `parent:` label linking them.

### Phase 7: Review & Merge Flow

21. **review-overlay** — When a card is in REVIEW column and user
    presses Enter:
    - **Diff tab**: Shows git diff from structured result (syntax
      highlighted with lipgloss).
    - **AI Review tab**: Shows review summary if available.
    - **Actions**: `[a]pprove` moves to DONE, `[r]eject` sends back to
      IN_PROGRESS with feedback prompt.

22. **merge-action** — Press `m` on a REVIEW card to trigger merge.
    If task has `pushBranch`, calls `create_pull_request` /
    `merge_pull_request` coordination tools via the API, then moves
    card to DONE.

### Phase 8: Actions Palette & Search

23. **actions-palette** — `internal/cli/tui/palette.go`. Press `.` or
    `Ctrl+P` to open a fuzzy-searchable command palette overlay:
    - Lists all available actions for current context (board-level,
      card-level, global).
    - Type to filter. Enter to execute.
    - Actions: New Task, Plan, Run Agent, View Logs, Delete, Move,
      Sync Project, Settings, Quit.

24. **search** — Press `/` on the board to open search bar. Fuzzy-filter
    cards across all columns by name, description, agent. Matching
    cards highlighted, non-matching dimmed.

### Phase 9: Polish & Tests

25. **responsive-layout** — Handle narrow terminals gracefully:
    - <120 cols: show 2 columns at a time with H/L horizontal scroll.
    - <80 cols: single-column view with Tab column switching.
    - Minimum 80×20 terminal size requirement.

26. **toast-notifications** — `internal/cli/tui/toast.go`. Transient
    notifications that appear at top of board and auto-dismiss after 3s:
    - Task phase transitions ("backend-dev: Running → Succeeded").
    - Errors ("Failed to sync with GitHub project").
    - Confirmations ("Task created", "Moved to REVIEW").

27. **config-improvements** — Extend `mercan config`:
    - `mercan config set-project <owner/number>` — persist GitHub project.
    - `mercan config set-github-token <token>` — persist GitHub token.
    - `mercan config view` — show all config (mask tokens).

28. **tests** — Unit tests for:
    - GitHub Projects GraphQL client (mock HTTP responses).
    - Board model updates (card navigation, column transitions).
    - Card rendering (various states, widths).
    - Plan mode parsing.
    - Sync logic (project ↔ Mercan reconciliation).
    - Existing API client and SSE parser tests remain.

## Dependencies to Add

```
github.com/hasura/go-graphql-client   # GitHub Projects v2 GraphQL
  — or —
github.com/shurcooL/graphql           # Alternative GraphQL client

# Existing (already in go.mod)
github.com/spf13/cobra
github.com/charmbracelet/bubbletea
github.com/charmbracelet/lipgloss
github.com/charmbracelet/bubbles
```

## File Layout (New/Changed Files)

```
cmd/cli/
  main.go              # Add `mercan` (no subcommand) → launches board
  run.go               # Updated: creates task + opens board (TTY) or plain-text (non-TTY)
  plan.go              # NEW: `mercan plan` subcommand
  config.go            # Updated: add set-project, set-github-token

internal/cli/
  github/
    projects.go        # NEW: GitHub Projects v2 GraphQL client
    sync.go            # NEW: Bidirectional sync (project ↔ Mercan tasks)
    types.go           # NEW: Project item types, field mappings
    projects_test.go   # NEW: Tests with mock GraphQL responses
    sync_test.go       # NEW: Sync logic tests

  tui/
    board.go           # NEW: Kanban board root model (replaces model.go as default)
    column.go          # NEW: Column component (header + scrollable card list)
    card.go            # NEW: Card component (bordered, status-aware)
    detail.go          # NEW: Task detail full-screen overlay (tabs)
    plan.go            # NEW: Plan mode input + approval UI
    palette.go         # NEW: Actions palette (fuzzy command search)
    search.go          # NEW: Board search bar + card filtering
    header.go          # NEW: Top header bar (logo, branch, project info)
    toast.go           # NEW: Transient notification system
    model.go           # KEEP: Coordinator streaming view (used inside detail overlay)
    messages.go        # UPDATED: Add BoardUpdateMsg, PlanProposalMsg, ToastMsg, etc.
    styles.go          # UPDATED: Kagan-style card borders, column headers, badges
    poller.go          # UPDATED: Refactored into board_poller.go
    spinner.go         # KEEP: unchanged
    board_test.go      # NEW: Board model update tests
    card_test.go       # NEW: Card rendering tests
```

## Style Guide (Kagan Alignment)

| Element          | Style                                                      |
|------------------|------------------------------------------------------------|
| Card border      | `lipgloss.RoundedBorder()`, default gray `243`             |
| Card focused     | Cyan border `39`, bold title                               |
| Card running     | Animated: alternate blue `33` / cyan `39` on `TickMsg`     |
| Card succeeded   | Green left-accent `42`                                     |
| Card failed      | Red left-accent `196`                                      |
| Column header    | Bold, uppercase, dim separator line below                  |
| Status icons     | ⏸ Pending (gray), ▶ Running (blue), ✓ Done (green), ✗ Failed (red) |
| Priority badges  | P0 red bg, P1 orange bg, P2 yellow bg, P3 gray bg         |
| Type badges      | `ai` cyan, `agent` magenta, `container` green              |
| Header bar       | Bold title left, branch/info right, full-width bg `236`    |
| Status bar       | Gray `243`, keybinding hints, spinner when active          |
| Peek overlay     | Double border, cyan, padded (existing)                     |
| Toast            | Top-right, rounded border, auto-dismiss, colored by type   |
| Actions palette  | Centered overlay, search input + filtered action list      |

## Notes

- The existing `mercan run` plain-text mode (non-TTY) is unchanged —
  it still streams SSE events as plain text for CI/pipe usage.
- In TTY mode, `mercan run` now creates a task and opens the board
  with that task focused, rather than showing the old two-pane view.
- The old two-pane coordinator view (`model.go`) is preserved and
  reused inside the task detail overlay's Logs tab for streaming output.
- GitHub Projects integration is optional — if `--project` is not set
  and no project is configured, the board pulls tasks directly from the
  Mercan API and synthesizes columns from task phases.
- The GraphQL client needs a GitHub token with `project` scope. The
  Mercan API token (kubeconfig) is separate.
- SSE client remains POST-based to `/api/v1/chat` for coordinator
  streaming — this feeds the detail overlay, not the board itself.
- Board-level polling uses REST `GET /api/v1/tasks` — same as the
  current agent poller but without parent-task filter.
- Plan mode reuses the chat endpoint with a specialized system prompt
  that outputs structured JSON; the CLI parses this into task proposals.
