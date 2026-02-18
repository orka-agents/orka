import * as vscode from 'vscode';
import { orkaApi, Task } from '../api/client';

const STATUS_ORDER = ['Running', 'Pending', 'Succeeded', 'Failed', 'Cancelled'];
const STATUS_ICONS: Record<string, string> = {
  Running: 'sync~spin',
  Pending: 'clock',
  Succeeded: 'pass',
  Failed: 'error',
  Cancelled: 'close',
};

export class TaskTreeProvider implements vscode.TreeDataProvider<TaskTreeItem> {
  private _onDidChangeTreeData = new vscode.EventEmitter<TaskTreeItem | undefined | null | void>();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;

  private tasks: Task[] = [];

  refresh(): void {
    this._onDidChangeTreeData.fire();
  }

  async getChildren(element?: TaskTreeItem): Promise<TaskTreeItem[]> {
    if (!orkaApi.isConfigured) {
      return [new TaskTreeItem('Not connected', '', vscode.TreeItemCollapsibleState.None)];
    }

    if (!element) {
      try {
        this.tasks = await orkaApi.listTasks();
        const groups: TaskTreeItem[] = [];
        for (const status of STATUS_ORDER) {
          const tasksInGroup = this.tasks.filter(t => (t.status?.phase || 'Pending') === status);
          if (tasksInGroup.length > 0) {
            const item = new TaskTreeItem(
              `${status} (${tasksInGroup.length})`,
              '',
              vscode.TreeItemCollapsibleState.Expanded
            );
            item.iconPath = new vscode.ThemeIcon(STATUS_ICONS[status] || 'circle');
            item.contextValue = 'taskGroup';
            groups.push(item);
          }
        }
        if (groups.length === 0) {
          return [new TaskTreeItem('No tasks found', '', vscode.TreeItemCollapsibleState.None)];
        }
        return groups;
      } catch (err: any) {
        return [new TaskTreeItem('Error loading tasks', err.message, vscode.TreeItemCollapsibleState.None)];
      }
    }

    // Children: tasks in this status group
    const statusMatch = element.label?.toString().match(/^(\w+)\s*\(/);
    if (statusMatch) {
      const status = statusMatch[1];
      return this.tasks
        .filter(t => (t.status?.phase || 'Pending') === status)
        .map(task => {
          const duration = this.formatDuration(task);
          const desc = [task.spec.type, duration].filter(Boolean).join(' • ');
          const item = new TaskTreeItem(
            task.metadata.name,
            desc,
            vscode.TreeItemCollapsibleState.None
          );
          item.contextValue = 'task';
          item.iconPath = new vscode.ThemeIcon(STATUS_ICONS[task.status?.phase || 'Pending'] || 'circle');
          item.command = {
            command: 'orka.viewTaskDetail',
            title: 'View Task Detail',
            arguments: [task.metadata.name],
          };
          return item;
        });
    }

    return [];
  }

  getTreeItem(element: TaskTreeItem): vscode.TreeItem {
    return element;
  }

  private formatDuration(task: Task): string {
    const start = task.status?.startTime ? new Date(task.status.startTime).getTime() : 0;
    if (!start) { return ''; }
    const end = task.status?.completionTime
      ? new Date(task.status.completionTime).getTime()
      : Date.now();
    const seconds = Math.floor((end - start) / 1000);
    if (seconds < 60) { return `${seconds}s`; }
    const minutes = Math.floor(seconds / 60);
    const secs = seconds % 60;
    return `${minutes}m ${secs}s`;
  }
}

export class TaskTreeItem extends vscode.TreeItem {
  constructor(
    public readonly label: string,
    public readonly description: string,
    public readonly collapsibleState: vscode.TreeItemCollapsibleState
  ) {
    super(label, collapsibleState);
  }
}
