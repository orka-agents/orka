import { useNavigate } from '@tanstack/react-router'
import { cn } from '@/lib/utils'
import { phaseStyle, typeStyle } from '@/lib/task-status'
import { StatusDot } from '@/components/ui/status-dot'
import type { Task, TaskPhase, TaskType } from '@/schemas/task'

/** A node in the delegation graph. */
interface GraphNode {
  name: string
  /** Agent that owns/handles this node (root may be undefined). */
  agent?: string
  phase?: TaskPhase | string
  type?: TaskType | string
  children: GraphNode[]
}

/**
 * Build a graph from a parent task plus its (currently flat) `childTasks`.
 *
 * The Orka API exposes one delegation level (`status.childTasks[]`), so v1 is a
 * root with a single tier of children. The shape is recursive (`children[]`)
 * so this can upgrade to a true multi-level DAG later without changing the
 * component API.
 */
function buildGraph(task: Task): GraphNode {
  const children = (task.status?.childTasks ?? []).map((c) => ({
    name: c.name,
    agent: c.agent,
    phase: c.phase,
    children: [] as GraphNode[],
  }))
  return {
    name: task.metadata.name,
    agent: task.spec.agentRef?.name,
    phase: task.status?.phase,
    type: task.spec.type,
    children,
  }
}

function TreeNode({
  node,
  depth,
  onNavigate,
}: {
  node: GraphNode
  depth: number
  onNavigate: (name: string) => void
}) {
  const phase = phaseStyle(node.phase)
  // Preserve the literal phase string for display/AT (matching StatusDot's
  // contract); only the *styling* falls back to Pending for unknown phases.
  const phaseLabel =
    typeof node.phase === 'string' && node.phase ? node.phase : phase.label
  const type = node.type ? typeStyle(node.type) : undefined
  const TypeIcon = type?.icon
  const hasChildren = node.children.length > 0

  return (
    <li role="treeitem" aria-label={`${node.name} (${phaseLabel})`} className="relative">
      <button
        type="button"
        onClick={() => onNavigate(node.name)}
        style={{ paddingLeft: `${depth * 1.25 + 0.5}rem` }}
        className={cn(
          'group flex w-full items-center gap-2 rounded-md border-l-2 py-1.5 pr-2 text-left transition-colors',
          'hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring',
          phase.railClass,
        )}
      >
        <StatusDot phase={node.phase} hideLabel />
        {TypeIcon && (
          <TypeIcon className={cn('size-3.5 shrink-0', type?.textClass)} aria-hidden="true" />
        )}
        <span className="truncate font-mono text-xs font-medium">{node.name}</span>
        {node.agent && (
          <span className="truncate text-xs text-muted-foreground">{node.agent}</span>
        )}
        <span className={cn('ml-auto shrink-0 text-xs font-medium', phase.textClass)}>
          {phaseLabel}
        </span>
      </button>
      {hasChildren && (
        <ul role="group" className="space-y-1">
          {node.children.map((child) => (
            <TreeNode key={child.name} node={child} depth={depth + 1} onNavigate={onNavigate} />
          ))}
        </ul>
      )}
    </li>
  )
}

interface ExecutionGraphProps {
  task: Task
  /** Optional navigation override (defaults to TanStack router). */
  onSelect?: (taskName: string) => void
  className?: string
}

/**
 * Live delegation graph for a task and its child tasks.
 *
 * v1 is an indented tree with phase-colored connector rails (no graph-layout
 * dependency), structured so it can upgrade to a true DAG later. Nodes are
 * colored by phase (shared status module), tagged with the task-type icon, and
 * named in monospace; running nodes pulse via the shared <StatusDot>. Clicking
 * a node routes to that task.
 *
 * Degrades gracefully: a task with no children renders just the root node.
 */
export function ExecutionGraph({ task, onSelect, className }: ExecutionGraphProps) {
  const navigate = useNavigate()
  const root = buildGraph(task)

  const handleNavigate = (name: string) => {
    if (onSelect) onSelect(name)
    else navigate({ to: '/tasks/$taskId', params: { taskId: name } })
  }

  return (
    <ul
      role="tree"
      aria-label="Task execution graph"
      className={cn('space-y-1', className)}
    >
      <TreeNode node={root} depth={0} onNavigate={handleNavigate} />
    </ul>
  )
}
