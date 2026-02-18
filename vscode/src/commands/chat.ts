import * as vscode from 'vscode';
import { orkaApi } from '../api/client';

export async function openChat(sessionId?: string): Promise<void> {
  if (!orkaApi.isConfigured) {
    vscode.window.showWarningMessage('Not connected to Orka. Run "Orka: Connect" first.');
    return;
  }

  // Delegate to the chat panel
  vscode.commands.executeCommand('orka.openChatPanel', sessionId);
}
