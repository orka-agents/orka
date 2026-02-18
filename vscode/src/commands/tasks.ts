import * as vscode from 'vscode';
import { orkaApi, CreateTaskRequest } from '../api/client';

export async function createTask(): Promise<void> {
  if (!orkaApi.isConfigured) {
    vscode.window.showWarningMessage('Not connected to Orka. Run "Orka: Connect" first.');
    return;
  }

  const taskType = await vscode.window.showQuickPick(
    [
      { label: '$(hubot) AI Agent Task', description: 'Run a task using an Orka agent', value: 'agent' as const },
      { label: '$(beaker) AI Task (one-shot)', description: 'Single LLM call without agent', value: 'ai' as const },
      { label: '$(package) Container Task', description: 'Run a container image', value: 'container' as const },
    ],
    { placeHolder: 'Select task type', title: 'Create Orka Task' }
  );
  if (!taskType) return;

  const name = await vscode.window.showInputBox({
    prompt: 'Enter task name',
    placeHolder: 'my-task',
    validateInput: (v) => /^[a-z0-9][a-z0-9-]*[a-z0-9]$/.test(v) ? null : 'Must be a valid Kubernetes name (lowercase, alphanumeric, hyphens)',
  });
  if (!name) return;

  const req: CreateTaskRequest = { name, spec: { type: taskType.value } };

  if (taskType.value === 'agent') {
    try {
      const agents = await orkaApi.listAgents();
      if (agents.length === 0) {
        vscode.window.showWarningMessage('No agents available. Create an agent first.');
        return;
      }
      const agentPick = await vscode.window.showQuickPick(
        agents.map(a => ({
          label: `$(hubot) ${a.metadata.name}`,
          description: [a.spec.provider, a.spec.model].filter(Boolean).join(' / '),
          value: a.metadata.name,
        })),
        { placeHolder: 'Select agent', title: 'Select Agent' }
      );
      if (!agentPick) return;
      req.spec.agentRef = agentPick.value;
    } catch (err: any) {
      vscode.window.showErrorMessage(`Failed to list agents: ${err.message}`);
      return;
    }

    const prompt = await vscode.window.showInputBox({
      prompt: 'Enter prompt for the agent',
      placeHolder: 'Review the auth module for security vulnerabilities',
      ignoreFocusOut: true,
    });
    if (!prompt) return;
    req.spec.prompt = prompt;

  } else if (taskType.value === 'ai') {
    const prompt = await vscode.window.showInputBox({
      prompt: 'Enter prompt',
      placeHolder: 'Summarize the README.md file',
      ignoreFocusOut: true,
    });
    if (!prompt) return;
    req.spec.prompt = prompt;

  } else if (taskType.value === 'container') {
    const image = await vscode.window.showInputBox({
      prompt: 'Enter container image',
      placeHolder: 'alpine:latest',
    });
    if (!image) return;
    req.spec.image = image;

    const command = await vscode.window.showInputBox({
      prompt: 'Enter command (optional, space-separated)',
      placeHolder: 'echo hello world',
    });
    if (command) {
      req.spec.command = command.split(' ');
    }
  }

  try {
    const task = await orkaApi.createTask(req);
    vscode.window.showInformationMessage(`Task "${task.metadata.name}" created`);
    vscode.commands.executeCommand('orka.refreshTasks');
  } catch (err: any) {
    vscode.window.showErrorMessage(`Failed to create task: ${err.message}`);
  }
}

export async function cancelTask(taskName?: string): Promise<void> {
  if (!taskName) {
    taskName = await pickTask('Select task to cancel', ['Running', 'Pending']);
  }
  if (!taskName) return;

  try {
    await orkaApi.cancelTask(taskName);
    vscode.window.showInformationMessage(`Task "${taskName}" cancelled`);
    vscode.commands.executeCommand('orka.refreshTasks');
  } catch (err: any) {
    vscode.window.showErrorMessage(`Failed to cancel task: ${err.message}`);
  }
}

export async function deleteTask(taskName?: string): Promise<void> {
  if (!taskName) {
    taskName = await pickTask('Select task to delete');
  }
  if (!taskName) return;

  const confirm = await vscode.window.showWarningMessage(
    `Delete task "${taskName}"?`,
    { modal: true },
    'Delete'
  );
  if (confirm !== 'Delete') return;

  try {
    await orkaApi.deleteTask(taskName);
    vscode.window.showInformationMessage(`Task "${taskName}" deleted`);
    vscode.commands.executeCommand('orka.refreshTasks');
  } catch (err: any) {
    vscode.window.showErrorMessage(`Failed to delete task: ${err.message}`);
  }
}

export async function runWithOrka(): Promise<void> {
  const editor = vscode.window.activeTextEditor;
  if (!editor) {
    vscode.window.showWarningMessage('No active editor');
    return;
  }

  const selection = editor.selection;
  const text = editor.document.getText(selection.isEmpty ? undefined : selection);
  if (!text.trim()) {
    vscode.window.showWarningMessage('No text selected');
    return;
  }

  if (!orkaApi.isConfigured) {
    vscode.window.showWarningMessage('Not connected to Orka');
    return;
  }

  try {
    const agents = await orkaApi.listAgents();
    if (agents.length === 0) {
      vscode.window.showWarningMessage('No agents available');
      return;
    }

    const agentPick = await vscode.window.showQuickPick(
      agents.map(a => ({
        label: `$(hubot) ${a.metadata.name}`,
        description: [a.spec.provider, a.spec.model].filter(Boolean).join(' / '),
        value: a.metadata.name,
      })),
      { placeHolder: 'Select agent to run with' }
    );
    if (!agentPick) return;

    const name = `run-${Date.now()}`;
    const task = await orkaApi.createTask({
      name,
      spec: {
        type: 'agent',
        agentRef: agentPick.value,
        prompt: text,
      },
    });

    vscode.window.showInformationMessage(`Task "${task.metadata.name}" created with ${agentPick.value}`);
    vscode.commands.executeCommand('orka.refreshTasks');
    vscode.commands.executeCommand('orka.viewTaskDetail', task.metadata.name);
  } catch (err: any) {
    vscode.window.showErrorMessage(`Failed to create task: ${err.message}`);
  }
}

async function pickTask(title: string, phases?: string[]): Promise<string | undefined> {
  try {
    let tasks = await orkaApi.listTasks();
    if (phases) {
      tasks = tasks.filter(t => phases.includes(t.status?.phase || 'Pending'));
    }
    if (tasks.length === 0) {
      vscode.window.showInformationMessage('No matching tasks found');
      return;
    }
    const pick = await vscode.window.showQuickPick(
      tasks.map(t => ({
        label: t.metadata.name,
        description: `${t.spec.type} • ${t.status?.phase || 'Pending'}`,
        value: t.metadata.name,
      })),
      { placeHolder: title }
    );
    return pick?.value;
  } catch (err: any) {
    vscode.window.showErrorMessage(`Failed to list tasks: ${err.message}`);
    return;
  }
}
