import * as vscode from 'vscode';
import { orkaApi, CreateAgentRequest } from '../api/client';

export async function createAgent(): Promise<void> {
  if (!orkaApi.isConfigured) {
    vscode.window.showWarningMessage('Not connected to Orka. Run "Orka: Connect" first.');
    return;
  }

  const name = await vscode.window.showInputBox({
    prompt: 'Enter agent name',
    placeHolder: 'my-agent',
    validateInput: (v) => /^[a-z0-9][a-z0-9-]*[a-z0-9]$/.test(v) ? null : 'Must be a valid Kubernetes name',
  });
  if (!name) return;

  const provider = await vscode.window.showInputBox({
    prompt: 'Enter provider (e.g., openai, anthropic)',
    placeHolder: 'openai',
  });

  const model = await vscode.window.showInputBox({
    prompt: 'Enter model name',
    placeHolder: 'gpt-4o',
  });

  const systemPrompt = await vscode.window.showInputBox({
    prompt: 'Enter system prompt (optional)',
    placeHolder: 'You are a helpful coding assistant.',
    ignoreFocusOut: true,
  });

  const req: CreateAgentRequest = {
    name,
    spec: {
      provider: provider || undefined,
      model: model || undefined,
      systemPrompt: systemPrompt || undefined,
    },
  };

  try {
    await orkaApi.createAgent(req);
    vscode.window.showInformationMessage(`Agent "${name}" created`);
    vscode.commands.executeCommand('orka.refreshAgents');
  } catch (err: any) {
    vscode.window.showErrorMessage(`Failed to create agent: ${err.message}`);
  }
}

export async function deleteAgent(agentName?: string): Promise<void> {
  if (!agentName) {
    try {
      const agents = await orkaApi.listAgents();
      if (agents.length === 0) {
        vscode.window.showInformationMessage('No agents found');
        return;
      }
      const pick = await vscode.window.showQuickPick(
        agents.map(a => ({
          label: a.metadata.name,
          description: [a.spec.provider, a.spec.model].filter(Boolean).join(' / '),
          value: a.metadata.name,
        })),
        { placeHolder: 'Select agent to delete' }
      );
      if (!pick) return;
      agentName = pick.value;
    } catch (err: any) {
      vscode.window.showErrorMessage(`Failed to list agents: ${err.message}`);
      return;
    }
  }

  const confirm = await vscode.window.showWarningMessage(
    `Delete agent "${agentName}"?`,
    { modal: true },
    'Delete'
  );
  if (confirm !== 'Delete') return;

  try {
    await orkaApi.deleteAgent(agentName);
    vscode.window.showInformationMessage(`Agent "${agentName}" deleted`);
    vscode.commands.executeCommand('orka.refreshAgents');
  } catch (err: any) {
    vscode.window.showErrorMessage(`Failed to delete agent: ${err.message}`);
  }
}
