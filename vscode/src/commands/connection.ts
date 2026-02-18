import * as vscode from 'vscode';
import { orkaApi } from '../api/client';
import { orkaAuth } from '../api/auth';
import { getConnectionSettings } from '../utils/config';
import { detectFromKubeconfig } from '../utils/kubeconfig';

let statusBarItem: vscode.StatusBarItem;
let pollingInterval: ReturnType<typeof setInterval> | undefined;
let refreshCallbacks: (() => void)[] = [];

export function createStatusBarItem(): vscode.StatusBarItem {
  statusBarItem = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 100);
  statusBarItem.command = 'orka.connect';
  updateStatusBar(false);
  statusBarItem.show();
  return statusBarItem;
}

export function onRefresh(callback: () => void): void {
  refreshCallbacks.push(callback);
}

function updateStatusBar(connected: boolean, info?: string): void {
  if (!statusBarItem) return;
  if (connected) {
    statusBarItem.text = `$(check) Orka: ${info || 'connected'}`;
    statusBarItem.tooltip = 'Connected to Orka. Click to reconnect.';
    statusBarItem.backgroundColor = undefined;
  } else {
    statusBarItem.text = '$(error) Orka: disconnected';
    statusBarItem.tooltip = 'Not connected to Orka. Click to connect.';
    statusBarItem.backgroundColor = new vscode.ThemeColor('statusBarItem.errorBackground');
  }
}

export async function connect(): Promise<boolean> {
  const settings = getConnectionSettings();
  let endpoint = settings.apiEndpoint;
  let token = settings.authToken || orkaAuth.getToken();
  let namespace = settings.namespace;

  // Try kubeconfig fallback
  if (!endpoint && settings.useKubeconfig) {
    const kubeInfo = await detectFromKubeconfig();
    if (kubeInfo) {
      endpoint = kubeInfo.apiEndpoint || '';
      token = token || kubeInfo.authToken || '';
      namespace = kubeInfo.namespace || namespace;
    }
  }

  if (!endpoint) {
    const input = await vscode.window.showInputBox({
      prompt: 'Enter Orka API endpoint URL',
      placeHolder: 'http://localhost:8080',
      ignoreFocusOut: true,
    });
    if (!input) return false;
    endpoint = input;
    await vscode.workspace.getConfiguration('orka').update('apiEndpoint', endpoint, vscode.ConfigurationTarget.Global);
  }

  orkaApi.configure(endpoint, token, namespace);

  try {
    updateStatusBar(false, 'connecting...');
    statusBarItem.text = '$(sync~spin) Orka: connecting...';

    if (token) {
      await orkaApi.validateAuth();
    }
    const agents = await orkaApi.listAgents();
    const tasks = await orkaApi.listTasks();
    const runningCount = tasks.filter(t => t.status?.phase === 'Running').length;

    const info = `${agents.length} agents${runningCount > 0 ? ` • ${runningCount} running` : ' • idle'}`;
    updateStatusBar(true, info);

    startPolling();

    vscode.window.showInformationMessage(`Connected to Orka at ${endpoint}`);
    return true;
  } catch (err: any) {
    updateStatusBar(false);
    vscode.window.showErrorMessage(`Failed to connect to Orka: ${err.message}`);
    return false;
  }
}

export function disconnect(): void {
  stopPolling();
  orkaApi.configure('', '', 'default');
  updateStatusBar(false);
  refreshCallbacks.forEach(cb => cb());
  vscode.window.showInformationMessage('Disconnected from Orka');
}

function startPolling(): void {
  stopPolling();
  const settings = getConnectionSettings();
  pollingInterval = setInterval(async () => {
    try {
      const tasks = await orkaApi.listTasks();
      const agents = await orkaApi.listAgents();
      const runningCount = tasks.filter(t => t.status?.phase === 'Running').length;
      const info = `${agents.length} agents${runningCount > 0 ? ` • ${runningCount} running` : ' • idle'}`;
      updateStatusBar(true, info);
      refreshCallbacks.forEach(cb => cb());
    } catch {
      updateStatusBar(false);
    }
  }, settings.refreshInterval);
}

function stopPolling(): void {
  if (pollingInterval) {
    clearInterval(pollingInterval);
    pollingInterval = undefined;
  }
}

export function disposeConnection(): void {
  stopPolling();
  statusBarItem?.dispose();
}
