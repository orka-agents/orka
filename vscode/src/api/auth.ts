import * as vscode from 'vscode';
import { getConfig } from '../utils/config';

export class OrkaAuth {
  private token: string = '';

  /**
   * Get the current auth token from settings or stored value
   */
  getToken(): string {
    const configToken = getConfig<string>('authToken', '');
    return configToken || this.token;
  }

  /**
   * Set the auth token (runtime override)
   */
  setToken(token: string): void {
    this.token = token;
  }

  /**
   * Prompt user for auth token and save to settings
   */
  async promptForToken(): Promise<string | undefined> {
    const token = await vscode.window.showInputBox({
      prompt: 'Enter your Orka auth token',
      password: true,
      placeHolder: 'Bearer token for Orka API',
      ignoreFocusOut: true,
    });

    if (token) {
      await vscode.workspace.getConfiguration('orka').update('authToken', token, vscode.ConfigurationTarget.Global);
      this.token = token;
    }

    return token;
  }

  /**
   * Clear stored token
   */
  async clearToken(): Promise<void> {
    this.token = '';
    await vscode.workspace.getConfiguration('orka').update('authToken', '', vscode.ConfigurationTarget.Global);
  }

  /**
   * Check if a token is available
   */
  get hasToken(): boolean {
    return this.getToken().length > 0;
  }
}

export const orkaAuth = new OrkaAuth();
