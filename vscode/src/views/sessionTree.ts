import * as vscode from 'vscode';
import { orkaApi, Session } from '../api/client';

export class SessionTreeProvider implements vscode.TreeDataProvider<SessionTreeItem> {
  private _onDidChangeTreeData = new vscode.EventEmitter<SessionTreeItem | undefined | null | void>();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;

  refresh(): void {
    this._onDidChangeTreeData.fire();
  }

  async getChildren(element?: SessionTreeItem): Promise<SessionTreeItem[]> {
    if (!orkaApi.isConfigured) {
      return [new SessionTreeItem('Not connected', '', vscode.TreeItemCollapsibleState.None)];
    }

    if (element) { return []; }

    try {
      const sessions = await orkaApi.listSessions();
      if (sessions.length === 0) {
        return [new SessionTreeItem('No sessions', '', vscode.TreeItemCollapsibleState.None)];
      }

      return sessions.map(session => {
        const parts = [
          session.agentRef,
          session.model,
          session.messageCount ? `${session.messageCount} msgs` : null,
        ].filter(Boolean);
        const desc = parts.join(' • ');

        const item = new SessionTreeItem(
          session.id,
          desc,
          vscode.TreeItemCollapsibleState.None
        );
        item.iconPath = new vscode.ThemeIcon('comment-discussion');
        item.contextValue = 'session';
        item.tooltip = [
          `Session: ${session.id}`,
          session.provider ? `Provider: ${session.provider}` : null,
          session.model ? `Model: ${session.model}` : null,
          session.totalTokens ? `Tokens: ${session.totalTokens}` : null,
          session.createdAt ? `Created: ${new Date(session.createdAt).toLocaleString()}` : null,
        ].filter(Boolean).join('\n');
        item.command = {
          command: 'orka.openChat',
          title: 'Open Chat',
          arguments: [session.id],
        };
        return item;
      });
    } catch (err: any) {
      return [new SessionTreeItem('Error loading sessions', err.message, vscode.TreeItemCollapsibleState.None)];
    }
  }

  getTreeItem(element: SessionTreeItem): vscode.TreeItem {
    return element;
  }
}

export class SessionTreeItem extends vscode.TreeItem {
  constructor(
    public readonly label: string,
    public readonly description: string,
    public readonly collapsibleState: vscode.TreeItemCollapsibleState
  ) {
    super(label, collapsibleState);
  }
}
