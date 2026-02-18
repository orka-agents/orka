import * as vscode from 'vscode';
import { orkaApi } from '../api/client.js';
import { OrkaSSEClient, ChatRequest, SSEMessageEvent, SSEToolCallEvent, SSEToolResultEvent, SSEDoneEvent, SSEStatusEvent } from '../api/sse.js';

export class ChatPanel {
  public static currentPanel: ChatPanel | undefined;
  private readonly panel: vscode.WebviewPanel;
  private readonly sseClient: OrkaSSEClient;
  private disposables: vscode.Disposable[] = [];
  private sessionId: string | undefined;
  private receivedContent: boolean = false;

  public static createOrShow(extensionUri: vscode.Uri, sessionId?: string): void {
    const column = vscode.ViewColumn.Beside;

    if (ChatPanel.currentPanel) {
      ChatPanel.currentPanel.panel.reveal(column);
      if (sessionId) {
        ChatPanel.currentPanel.sessionId = sessionId;
        ChatPanel.currentPanel.panel.webview.postMessage({ type: 'setSession', sessionId });
      }
      return;
    }

    const panel = vscode.window.createWebviewPanel(
      'orkaChat',
      'Orka Chat',
      column,
      {
        enableScripts: true,
        retainContextWhenHidden: true,
      }
    );

    ChatPanel.currentPanel = new ChatPanel(panel, extensionUri, sessionId);
  }

  private constructor(panel: vscode.WebviewPanel, _extensionUri: vscode.Uri, sessionId?: string) {
    this.panel = panel;
    this.sessionId = sessionId;
    this.sseClient = new OrkaSSEClient();

    this.panel.webview.html = this.getHtml();

    this.panel.webview.onDidReceiveMessage(
      async (message) => {
        switch (message.type) {
          case 'sendMessage':
            await this.sendMessage(message.text, message.agentRef, message.model);
            break;
          case 'cancelStream':
            this.sseClient.abort();
            break;
          case 'newChat':
            this.sessionId = undefined;
            this.panel.webview.postMessage({ type: 'clearChat' });
            break;
          case 'loadConfig':
            await this.loadConfig();
            break;
        }
      },
      null,
      this.disposables
    );

    this.panel.onDidDispose(() => this.dispose(), null, this.disposables);

    this.loadConfig();
  }

  private async loadConfig(): Promise<void> {
    try {
      const agents = await orkaApi.listAgents();
      const config = await orkaApi.getChatConfig();
      this.panel.webview.postMessage({
        type: 'config',
        agents: agents.map(a => ({ name: a.metadata.name, provider: a.spec.provider, model: a.spec.model })),
        models: config.models || [],
        providers: config.providers || [],
      });
    } catch {
      // Config loading is best-effort
    }
  }

  private async sendMessage(text: string, agentRef?: string, model?: string): Promise<void> {
    const { baseUrl, authToken } = orkaApi.getConnectionInfo();

    const request: ChatRequest = {
      message: text,
      sessionId: this.sessionId,
      agentRef,
      model,
    };

    this.sseClient.removeAllListeners();

    this.sseClient.on('status', (event: SSEStatusEvent) => {
      this.sessionId = event.sessionId;
      this.panel.webview.postMessage({ type: 'streamStatus', sessionId: event.sessionId });
    });

    this.sseClient.on('message', (event: SSEMessageEvent) => {
      this.receivedContent = true;
      this.panel.webview.postMessage({ type: 'streamContent', content: event.content });
    });

    this.sseClient.on('tool_call', (event: SSEToolCallEvent) => {
      this.panel.webview.postMessage({ type: 'toolCall', name: event.name, arguments: event.arguments, id: event.id });
    });

    this.sseClient.on('tool_result', (event: SSEToolResultEvent) => {
      this.panel.webview.postMessage({ type: 'toolResult', name: event.name, result: event.result, id: event.id });
    });

    this.sseClient.on('done', async (event: SSEDoneEvent) => {
      // If no message content was received, try to fetch the result from the last task
      if (!this.receivedContent && this.sessionId) {
        try {
          const tasks = await orkaApi.listTasks();
          const sessionTasks = tasks
            .filter(t => t.metadata.name.startsWith('chat-' + this.sessionId?.replace('chat-', '').substring(0, 7)))
            .filter(t => t.status?.phase === 'Succeeded' && !t.metadata.name.includes('-child-'));
          if (sessionTasks.length > 0) {
            const result = await orkaApi.getTaskResult(sessionTasks[0].metadata.name);
            if (result) {
              this.panel.webview.postMessage({ type: 'streamContent', content: result });
            }
          }
        } catch {
          // Best effort
        }
      }
      this.receivedContent = false;
      this.panel.webview.postMessage({ type: 'streamDone', usage: event.usage });
    });

    this.sseClient.on('error', (event: { message: string }) => {
      this.panel.webview.postMessage({ type: 'streamError', message: event.message });
    });

    this.panel.webview.postMessage({ type: 'streamStart' });
    await this.sseClient.chat(baseUrl, authToken, request);
  }

  private getHtml(): string {
    return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: var(--vscode-font-family);
      font-size: var(--vscode-font-size);
      color: var(--vscode-foreground);
      background: var(--vscode-editor-background);
      display: flex;
      flex-direction: column;
      height: 100vh;
    }
    .header {
      padding: 8px 16px;
      border-bottom: 1px solid var(--vscode-panel-border);
      display: flex;
      align-items: center;
      gap: 8px;
    }
    .header select {
      background: var(--vscode-dropdown-background);
      color: var(--vscode-dropdown-foreground);
      border: 1px solid var(--vscode-dropdown-border);
      padding: 4px 8px;
      border-radius: 4px;
      font-size: 12px;
    }
    .header button {
      background: var(--vscode-button-secondaryBackground);
      color: var(--vscode-button-secondaryForeground);
      border: none;
      padding: 4px 12px;
      border-radius: 4px;
      cursor: pointer;
      font-size: 12px;
    }
    .header button:hover {
      background: var(--vscode-button-secondaryHoverBackground);
    }
    .messages {
      flex: 1;
      overflow-y: auto;
      padding: 16px;
      display: flex;
      flex-direction: column;
      gap: 12px;
    }
    .message {
      max-width: 85%;
      padding: 10px 14px;
      border-radius: 8px;
      line-height: 1.5;
      white-space: pre-wrap;
      word-wrap: break-word;
    }
    .message.user {
      align-self: flex-end;
      background: var(--vscode-button-background);
      color: var(--vscode-button-foreground);
    }
    .message.assistant {
      align-self: flex-start;
      background: var(--vscode-editor-inactiveSelectionBackground);
    }
    .tool-card {
      margin: 4px 0;
      padding: 8px 12px;
      background: var(--vscode-textCodeBlock-background);
      border-radius: 6px;
      border-left: 3px solid var(--vscode-textLink-foreground);
      font-size: 12px;
      cursor: pointer;
    }
    .tool-card.pending { border-left-color: var(--vscode-charts-yellow); }
    .tool-card.complete { border-left-color: var(--vscode-charts-green); }
    .tool-card .tool-name { font-weight: bold; }
    .tool-card .tool-detail {
      display: none;
      margin-top: 6px;
      padding-top: 6px;
      border-top: 1px solid var(--vscode-panel-border);
      white-space: pre-wrap;
      max-height: 200px;
      overflow-y: auto;
    }
    .tool-card.expanded .tool-detail { display: block; }
    .usage {
      font-size: 11px;
      color: var(--vscode-descriptionForeground);
      padding: 4px 14px;
    }
    .input-area {
      padding: 12px 16px;
      border-top: 1px solid var(--vscode-panel-border);
      display: flex;
      gap: 8px;
    }
    .input-area textarea {
      flex: 1;
      background: var(--vscode-input-background);
      color: var(--vscode-input-foreground);
      border: 1px solid var(--vscode-input-border);
      padding: 8px 12px;
      border-radius: 6px;
      font-family: inherit;
      font-size: inherit;
      resize: none;
      min-height: 40px;
      max-height: 120px;
    }
    .input-area textarea:focus { outline: 1px solid var(--vscode-focusBorder); }
    .input-area button {
      background: var(--vscode-button-background);
      color: var(--vscode-button-foreground);
      border: none;
      padding: 8px 16px;
      border-radius: 6px;
      cursor: pointer;
      font-size: 14px;
      align-self: flex-end;
    }
    .input-area button:hover { background: var(--vscode-button-hoverBackground); }
    .input-area button:disabled { opacity: 0.5; cursor: default; }
    .welcome {
      flex: 1;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      gap: 8px;
      color: var(--vscode-descriptionForeground);
    }
    .welcome .icon { font-size: 48px; }
  </style>
</head>
<body>
  <div class="header">
    <span style="font-weight:bold;">&#x1F433; Orka Chat</span>
    <select id="agentSelect"><option value="">No agent</option></select>
    <select id="modelSelect"><option value="">Default model</option></select>
    <button onclick="newChat()">New Chat</button>
  </div>

  <div class="messages" id="messages">
    <div class="welcome">
      <div class="icon">&#x1F433;</div>
      <div>What can I help you with?</div>
    </div>
  </div>

  <div class="input-area">
    <textarea id="input" placeholder="Message the agent..." rows="1"
      onkeydown="if(event.key==='Enter'&&!event.shiftKey){event.preventDefault();send();}"></textarea>
    <button id="sendBtn" onclick="send()">&#x27A4;</button>
  </div>

  <script>
    const vscode = acquireVsCodeApi();
    const messagesEl = document.getElementById('messages');
    const inputEl = document.getElementById('input');
    const sendBtn = document.getElementById('sendBtn');
    const agentSelect = document.getElementById('agentSelect');
    const modelSelect = document.getElementById('modelSelect');

    let streaming = false;
    let currentAssistantEl = null;
    let currentContent = '';
    let hasToolCardsSinceLastContent = false;
    let welcomeShown = true;

    function send() {
      const text = inputEl.value.trim();
      if (!text || streaming) return;

      if (welcomeShown) {
        messagesEl.innerHTML = '';
        welcomeShown = false;
      }

      addMessage(text, 'user');
      inputEl.value = '';
      inputEl.style.height = 'auto';

      vscode.postMessage({
        type: 'sendMessage',
        text,
        agentRef: agentSelect.value || undefined,
        model: modelSelect.value || undefined,
      });
    }

    function newChat() {
      vscode.postMessage({ type: 'newChat' });
    }

    function addMessage(text, role) {
      const div = document.createElement('div');
      div.className = 'message ' + role;
      div.textContent = text;
      messagesEl.appendChild(div);
      messagesEl.scrollTop = messagesEl.scrollHeight;
      return div;
    }

    function addToolCard(name, detail, status) {
      const div = document.createElement('div');
      div.className = 'tool-card ' + status;
      div.innerHTML = '<div class="tool-name">' + (status === 'pending' ? '\\u2192 ' : '\\u2713 ') + escapeHtml(name) + '</div>' +
        '<div class="tool-detail">' + escapeHtml(detail) + '</div>';
      div.onclick = () => div.classList.toggle('expanded');
      messagesEl.appendChild(div);
      messagesEl.scrollTop = messagesEl.scrollHeight;
      return div;
    }

    function escapeHtml(text) {
      const div = document.createElement('div');
      div.textContent = text;
      return div.innerHTML;
    }

    // Auto-resize textarea
    inputEl.addEventListener('input', () => {
      inputEl.style.height = 'auto';
      inputEl.style.height = Math.min(inputEl.scrollHeight, 120) + 'px';
    });

    // Handle messages from extension
    window.addEventListener('message', (event) => {
      const msg = event.data;
      switch (msg.type) {
        case 'streamStart':
          streaming = true;
          sendBtn.disabled = true;
          sendBtn.textContent = '\\u23F9';
          sendBtn.onclick = () => vscode.postMessage({ type: 'cancelStream' });
          currentContent = '';
          hasToolCardsSinceLastContent = false;
          currentAssistantEl = addMessage('', 'assistant');
          break;

        case 'streamContent':
          if (hasToolCardsSinceLastContent) {
            currentContent = '';
            currentAssistantEl = addMessage('', 'assistant');
            hasToolCardsSinceLastContent = false;
          }
          currentContent += msg.content;
          if (currentAssistantEl) currentAssistantEl.textContent = currentContent;
          messagesEl.scrollTop = messagesEl.scrollHeight;
          break;

        case 'toolCall':
          hasToolCardsSinceLastContent = true;
          addToolCard(msg.name, 'Args: ' + (msg.arguments || ''), 'pending');
          break;

        case 'toolResult':
          hasToolCardsSinceLastContent = true;
          addToolCard(msg.name, 'Result: ' + (msg.result || '').substring(0, 500), 'complete');
          break;

        case 'streamDone':
          streaming = false;
          sendBtn.disabled = false;
          sendBtn.textContent = '\\u27A4';
          sendBtn.onclick = send;
          if (currentAssistantEl && !currentContent) {
            currentAssistantEl.remove();
          }
          if (msg.usage) {
            const usageEl = document.createElement('div');
            usageEl.className = 'usage';
            const parts = [];
            if (msg.usage.totalTokens) parts.push(msg.usage.totalTokens + ' tokens');
            if (msg.usage.promptTokens) parts.push(msg.usage.promptTokens + ' prompt');
            if (msg.usage.completionTokens) parts.push(msg.usage.completionTokens + ' completion');
            usageEl.textContent = '\\uD83D\\uDCCA ' + parts.join(' \\u2022 ');
            messagesEl.appendChild(usageEl);
          }
          currentAssistantEl = null;
          break;

        case 'streamError':
          streaming = false;
          sendBtn.disabled = false;
          sendBtn.textContent = '\\u27A4';
          sendBtn.onclick = send;
          const errEl = addMessage('Error: ' + msg.message, 'assistant');
          errEl.style.color = 'var(--vscode-errorForeground)';
          currentAssistantEl = null;
          break;

        case 'clearChat':
          messagesEl.innerHTML = '<div class="welcome"><div class="icon">&#x1F433;</div><div>What can I help you with?</div></div>';
          welcomeShown = true;
          break;

        case 'config':
          // Populate dropdowns
          agentSelect.innerHTML = '<option value="">No agent</option>';
          (msg.agents || []).forEach(a => {
            const opt = document.createElement('option');
            opt.value = a.name;
            opt.textContent = a.name + (a.model ? ' (' + a.model + ')' : '');
            agentSelect.appendChild(opt);
          });
          modelSelect.innerHTML = '<option value="">Default model</option>';
          (msg.models || []).forEach(m => {
            const opt = document.createElement('option');
            opt.value = m;
            opt.textContent = m;
            modelSelect.appendChild(opt);
          });
          break;

        case 'setSession':
          break;
      }
    });

    // Request initial config
    vscode.postMessage({ type: 'loadConfig' });
  </script>
</body>
</html>`;
  }

  public dispose(): void {
    ChatPanel.currentPanel = undefined;
    this.sseClient.abort();
    this.panel.dispose();
    while (this.disposables.length) {
      const d = this.disposables.pop();
      if (d) d.dispose();
    }
  }
}
