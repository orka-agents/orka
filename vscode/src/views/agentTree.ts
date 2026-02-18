import * as vscode from 'vscode';
import { orkaApi, Agent } from '../api/client';

export class AgentTreeProvider implements vscode.TreeDataProvider<AgentTreeItem> {
  private _onDidChangeTreeData = new vscode.EventEmitter<AgentTreeItem | undefined | null | void>();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;

  private agents: Agent[] = [];

  refresh(): void {
    this._onDidChangeTreeData.fire();
  }

  async getChildren(element?: AgentTreeItem): Promise<AgentTreeItem[]> {
    if (!orkaApi.isConfigured) {
      return [new AgentTreeItem('Not connected', '', 'Click to configure', vscode.TreeItemCollapsibleState.None)];
    }

    if (!element) {
      try {
        this.agents = await orkaApi.listAgents();
        if (this.agents.length === 0) {
          return [new AgentTreeItem('No agents found', '', 'Create an agent to get started', vscode.TreeItemCollapsibleState.None)];
        }
        return this.agents.map(agent => {
          const desc = [agent.spec.provider, agent.spec.model].filter(Boolean).join(' / ');
          const item = new AgentTreeItem(
            agent.metadata.name,
            desc,
            this.getAgentTooltip(agent),
            vscode.TreeItemCollapsibleState.Collapsed
          );
          item.contextValue = 'agent';
          item.iconPath = new vscode.ThemeIcon('hubot');
          item.command = {
            command: 'orka.viewAgentDetail',
            title: 'View Agent Detail',
            arguments: [agent.metadata.name],
          };
          return item;
        });
      } catch (err: any) {
        return [new AgentTreeItem('Error loading agents', '', err.message, vscode.TreeItemCollapsibleState.None)];
      }
    }

    // Children of an agent - show tools
    const agent = this.agents.find(a => a.metadata.name === element.label);
    if (agent?.spec.tools?.length) {
      return agent.spec.tools.map(tool => {
        const item = new AgentTreeItem(tool, '', `Tool: ${tool}`, vscode.TreeItemCollapsibleState.None);
        item.iconPath = new vscode.ThemeIcon('tools');
        return item;
      });
    }
    return [new AgentTreeItem('No tools configured', '', '', vscode.TreeItemCollapsibleState.None)];
  }

  getTreeItem(element: AgentTreeItem): vscode.TreeItem {
    return element;
  }

  private getAgentTooltip(agent: Agent): string {
    const lines = [`Agent: ${agent.metadata.name}`];
    if (agent.spec.provider) { lines.push(`Provider: ${agent.spec.provider}`); }
    if (agent.spec.model) { lines.push(`Model: ${agent.spec.model}`); }
    if (agent.spec.tools?.length) { lines.push(`Tools: ${agent.spec.tools.join(', ')}`); }
    return lines.join('\n');
  }
}

export class AgentTreeItem extends vscode.TreeItem {
  constructor(
    public readonly label: string,
    public readonly description: string,
    public readonly tooltip: string,
    public readonly collapsibleState: vscode.TreeItemCollapsibleState
  ) {
    super(label, collapsibleState);
  }
}
