import * as vscode from 'vscode';
import { orkaApi, Agent } from '../api/client.js';

export class AgentFormPanel {
  private readonly panel: vscode.WebviewPanel;
  private disposables: vscode.Disposable[] = [];

  public static show(extensionUri: vscode.Uri, agent?: Agent): void {
    const panel = vscode.window.createWebviewPanel(
      'orkaAgentForm',
      agent ? `Edit Agent: ${agent.metadata.name}` : 'Create Agent',
      vscode.ViewColumn.Active,
      { enableScripts: true }
    );
    new AgentFormPanel(panel, extensionUri, agent);
  }

  private constructor(panel: vscode.WebviewPanel, _extensionUri: vscode.Uri, agent?: Agent) {
    this.panel = panel;
    this.panel.webview.html = this.getHtml(agent);

    this.panel.webview.onDidReceiveMessage(
      async (message) => {
        if (message.type === 'save') {
          try {
            await orkaApi.createAgent({
              name: message.name,
              spec: {
                provider: message.provider || undefined,
                model: message.model || undefined,
                systemPrompt: message.systemPrompt || undefined,
                tools: message.tools ? message.tools.split(',').map((t: string) => t.trim()).filter(Boolean) : undefined,
                temperature: message.temperature ? parseFloat(message.temperature) : undefined,
                maxTokens: message.maxTokens ? parseInt(message.maxTokens) : undefined,
              },
            });
            vscode.window.showInformationMessage(`Agent "${message.name}" saved`);
            vscode.commands.executeCommand('orka.refreshAgents');
            this.panel.dispose();
          } catch (err: any) {
            vscode.window.showErrorMessage(`Failed to save agent: ${err.message}`);
          }
        }
      },
      null,
      this.disposables
    );

    this.panel.onDidDispose(() => {
      this.disposables.forEach(d => d.dispose());
    });
  }

  private getHtml(agent?: Agent): string {
    const name = agent?.metadata.name || '';
    const provider = agent?.spec.provider || '';
    const model = agent?.spec.model || '';
    const systemPrompt = agent?.spec.systemPrompt || '';
    const tools = agent?.spec.tools?.join(', ') || '';
    const temp = agent?.spec.temperature?.toString() || '';
    const maxTokens = agent?.spec.maxTokens?.toString() || '';
    const isEdit = agent ? true : false;

    return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: var(--vscode-font-family);
      color: var(--vscode-foreground);
      background: var(--vscode-editor-background);
      padding: 20px;
      max-width: 600px;
    }
    h2 { margin-bottom: 20px; }
    .field { margin-bottom: 16px; }
    label { display: block; margin-bottom: 4px; font-weight: 600; font-size: 12px; }
    input, textarea, select {
      width: 100%;
      background: var(--vscode-input-background);
      color: var(--vscode-input-foreground);
      border: 1px solid var(--vscode-input-border);
      padding: 8px 10px;
      border-radius: 4px;
      font-family: inherit;
      font-size: inherit;
    }
    textarea { min-height: 80px; resize: vertical; }
    .hint { font-size: 11px; color: var(--vscode-descriptionForeground); margin-top: 2px; }
    button {
      background: var(--vscode-button-background);
      color: var(--vscode-button-foreground);
      border: none;
      padding: 8px 20px;
      border-radius: 4px;
      cursor: pointer;
      font-size: 14px;
      margin-top: 8px;
    }
    button:hover { background: var(--vscode-button-hoverBackground); }
  </style>
</head>
<body>
  <h2>${isEdit ? '&#x270F;&#xFE0F; Edit' : '&#x2795; Create'} Agent</h2>
  <div class="field">
    <label>Name</label>
    <input id="name" value="${this.escapeHtml(name)}" ${isEdit ? 'readonly' : ''} placeholder="my-agent">
    <div class="hint">Lowercase, alphanumeric, hyphens only</div>
  </div>
  <div class="field">
    <label>Provider</label>
    <input id="provider" value="${this.escapeHtml(provider)}" placeholder="openai, anthropic, etc.">
  </div>
  <div class="field">
    <label>Model</label>
    <input id="model" value="${this.escapeHtml(model)}" placeholder="gpt-4o, claude-sonnet-4, etc.">
  </div>
  <div class="field">
    <label>System Prompt</label>
    <textarea id="systemPrompt" placeholder="You are a helpful assistant...">${this.escapeHtml(systemPrompt)}</textarea>
  </div>
  <div class="field">
    <label>Tools</label>
    <input id="tools" value="${this.escapeHtml(tools)}" placeholder="web_search, code_exec, file_read">
    <div class="hint">Comma-separated tool names</div>
  </div>
  <div class="field">
    <label>Temperature</label>
    <input id="temperature" type="number" step="0.1" min="0" max="2" value="${this.escapeHtml(temp)}" placeholder="0.7">
  </div>
  <div class="field">
    <label>Max Tokens</label>
    <input id="maxTokens" type="number" value="${this.escapeHtml(maxTokens)}" placeholder="4096">
  </div>
  <button onclick="save()">Save Agent</button>

  <script>
    const vscode = acquireVsCodeApi();
    function save() {
      vscode.postMessage({
        type: 'save',
        name: document.getElementById('name').value,
        provider: document.getElementById('provider').value,
        model: document.getElementById('model').value,
        systemPrompt: document.getElementById('systemPrompt').value,
        tools: document.getElementById('tools').value,
        temperature: document.getElementById('temperature').value,
        maxTokens: document.getElementById('maxTokens').value,
      });
    }
  </script>
</body>
</html>`;
  }

  private escapeHtml(text: string): string {
    return text
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }
}
