import * as vscode from 'vscode';
import { orkaApi, Tool } from '../api/client';

export class ToolTreeProvider implements vscode.TreeDataProvider<ToolTreeItem> {
  private _onDidChangeTreeData = new vscode.EventEmitter<ToolTreeItem | undefined | null | void>();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;

  private tools: Tool[] = [];

  refresh(): void {
    this._onDidChangeTreeData.fire();
  }

  async getChildren(element?: ToolTreeItem): Promise<ToolTreeItem[]> {
    if (!orkaApi.isConfigured) {
      return [new ToolTreeItem('Not connected', '', vscode.TreeItemCollapsibleState.None)];
    }

    if (!element) {
      try {
        this.tools = await orkaApi.listTools();
        const builtIn = this.tools.filter(t => t.spec?.builtIn);
        const custom = this.tools.filter(t => !t.spec?.builtIn);

        const groups: ToolTreeItem[] = [];
        if (builtIn.length) {
          const item = new ToolTreeItem('Built-in', `${builtIn.length} tools`, vscode.TreeItemCollapsibleState.Expanded);
          item.contextValue = 'toolGroup';
          groups.push(item);
        }
        if (custom.length) {
          const item = new ToolTreeItem('Custom', `${custom.length} tools`, vscode.TreeItemCollapsibleState.Expanded);
          item.contextValue = 'toolGroup';
          groups.push(item);
        }
        if (groups.length === 0) {
          return [new ToolTreeItem('No tools found', '', vscode.TreeItemCollapsibleState.None)];
        }
        return groups;
      } catch (err: any) {
        return [new ToolTreeItem('Error loading tools', err.message, vscode.TreeItemCollapsibleState.None)];
      }
    }

    // Filter by group
    const isBuiltIn = element.label === 'Built-in';
    return this.tools
      .filter(t => isBuiltIn ? t.spec?.builtIn : !t.spec?.builtIn)
      .map(tool => {
        const item = new ToolTreeItem(
          tool.metadata.name,
          tool.spec.description || '',
          vscode.TreeItemCollapsibleState.None
        );
        item.iconPath = new vscode.ThemeIcon('tools');
        item.tooltip = tool.spec.description || tool.metadata.name;
        item.contextValue = 'tool';
        return item;
      });
  }

  getTreeItem(element: ToolTreeItem): vscode.TreeItem {
    return element;
  }
}

export class ToolTreeItem extends vscode.TreeItem {
  constructor(
    public readonly label: string,
    public readonly description: string,
    public readonly collapsibleState: vscode.TreeItemCollapsibleState
  ) {
    super(label, collapsibleState);
  }
}
