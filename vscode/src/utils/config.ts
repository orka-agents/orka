import * as vscode from 'vscode';

/**
 * Get a configuration value from orka settings
 */
export function getConfig<T>(key: string, defaultValue: T): T {
  const config = vscode.workspace.getConfiguration('orka');
  return config.get<T>(key, defaultValue);
}

/**
 * Update a configuration value
 */
export async function setConfig(key: string, value: any, global: boolean = true): Promise<void> {
  const config = vscode.workspace.getConfiguration('orka');
  await config.update(key, value, global ? vscode.ConfigurationTarget.Global : vscode.ConfigurationTarget.Workspace);
}

/**
 * Get all Orka connection settings
 */
export function getConnectionSettings(): {
  apiEndpoint: string;
  authToken: string;
  namespace: string;
  autoConnect: boolean;
  refreshInterval: number;
  useKubeconfig: boolean;
} {
  return {
    apiEndpoint: getConfig<string>('apiEndpoint', 'http://localhost:8080'),
    authToken: getConfig<string>('authToken', ''),
    namespace: getConfig<string>('namespace', 'default'),
    autoConnect: getConfig<boolean>('autoConnect', true),
    refreshInterval: getConfig<number>('refreshInterval', 5000),
    useKubeconfig: getConfig<boolean>('useKubeconfig', true),
  };
}

/**
 * Listen for configuration changes
 */
export function onConfigChange(callback: (e: vscode.ConfigurationChangeEvent) => void): vscode.Disposable {
  return vscode.workspace.onDidChangeConfiguration((e) => {
    if (e.affectsConfiguration('orka')) {
      callback(e);
    }
  });
}
