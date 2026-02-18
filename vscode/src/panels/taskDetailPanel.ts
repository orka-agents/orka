import * as vscode from 'vscode';
import { orkaApi } from '../api/client.js';
import { OrkaSSEClient } from '../api/sse.js';

export class TaskDetailPanel {
  private static panels = new Map<string, TaskDetailPanel>();
  private readonly panel: vscode.WebviewPanel;
  private readonly sseClient: OrkaSSEClient;
  private disposables: vscode.Disposable[] = [];
  private taskName: string;

  public static createOrShow(extensionUri: vscode.Uri, taskName: string): void {
    const existing = TaskDetailPanel.panels.get(taskName);
    if (existing) {
      existing.panel.reveal();
      return;
    }

    const panel = vscode.window.createWebviewPanel(
      'orkaTaskDetail',
      `Task: ${taskName}`,
      vscode.ViewColumn.Active,
      { enableScripts: true, retainContextWhenHidden: true }
    );

    TaskDetailPanel.panels.set(taskName, new TaskDetailPanel(panel, extensionUri, taskName));
  }

  private constructor(panel: vscode.WebviewPanel, _extensionUri: vscode.Uri, taskName: string) {
    this.panel = panel;
    this.taskName = taskName;
    this.sseClient = new OrkaSSEClient();

    this.panel.webview.html = this.getHtml();
    this.loadTask();

    this.panel.webview.onDidReceiveMessage(
      async (message) => {
        switch (message.type) {
          case 'cancelTask':
            try {
              await orkaApi.cancelTask(this.taskName);
              vscode.commands.executeCommand('orka.refreshTasks');
              this.loadTask();
            } catch (err: any) {
              vscode.window.showErrorMessage(`Failed to cancel: ${err.message}`);
            }
            break;
          case 'switchTab':
            if (message.tab === 'logs') this.loadLogs();
            else if (message.tab === 'result') this.loadResult();
            else if (message.tab === 'plan') this.loadPlan();
            break;
          case 'refresh':
            this.loadTask();
            break;
        }
      },
      null,
      this.disposables
    );

    this.panel.onDidDispose(() => {
      TaskDetailPanel.panels.delete(this.taskName);
      this.sseClient.abort();
      this.disposables.forEach(d => d.dispose());
    });
  }

  private async loadTask(): Promise<void> {
    try {
      const task = await orkaApi.getTask(this.taskName);
      this.panel.webview.postMessage({ type: 'taskData', task });
      this.panel.title = `Task: ${this.taskName}`;
    } catch (err: any) {
      this.panel.webview.postMessage({ type: 'error', message: err.message });
    }
  }

  private async loadLogs(): Promise<void> {
    try {
      const logs = await orkaApi.getTaskLogs(this.taskName);
      this.panel.webview.postMessage({ type: 'logsData', logs });
    } catch (err: any) {
      this.panel.webview.postMessage({ type: 'logsData', logs: `Error: ${err.message}` });
    }
  }

  private async loadResult(): Promise<void> {
    try {
      const result = await orkaApi.getTaskResult(this.taskName);
      this.panel.webview.postMessage({ type: 'resultData', result });
    } catch (err: any) {
      this.panel.webview.postMessage({ type: 'resultData', result: `Error: ${err.message}` });
    }
  }

  private async loadPlan(): Promise<void> {
    try {
      const plan = await orkaApi.getTaskPlan(this.taskName);
      this.panel.webview.postMessage({ type: 'planData', plan });
    } catch (err: any) {
      this.panel.webview.postMessage({ type: 'planData', plan: null });
    }
  }

  private getHtml(): string {
    return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: var(--vscode-font-family);
      font-size: var(--vscode-font-size);
      color: var(--vscode-foreground);
      background: var(--vscode-editor-background);
      padding: 16px;
    }
    .header { margin-bottom: 16px; }
    .header h2 { margin-bottom: 4px; }
    .header .meta { color: var(--vscode-descriptionForeground); font-size: 12px; }
    .status-badge {
      display: inline-block;
      padding: 2px 8px;
      border-radius: 4px;
      font-size: 12px;
      font-weight: bold;
    }
    .status-Running { background: var(--vscode-charts-yellow); color: #000; }
    .status-Pending { background: var(--vscode-charts-blue); color: #fff; }
    .status-Succeeded { background: var(--vscode-charts-green); color: #fff; }
    .status-Failed { background: var(--vscode-charts-red); color: #fff; }
    .tabs {
      display: flex;
      gap: 4px;
      margin-bottom: 16px;
      border-bottom: 1px solid var(--vscode-panel-border);
    }
    .tab {
      padding: 8px 16px;
      cursor: pointer;
      border-bottom: 2px solid transparent;
      color: var(--vscode-descriptionForeground);
    }
    .tab.active {
      border-bottom-color: var(--vscode-focusBorder);
      color: var(--vscode-foreground);
    }
    .tab:hover { color: var(--vscode-foreground); }
    .content {
      background: var(--vscode-textCodeBlock-background);
      border-radius: 6px;
      padding: 12px;
      font-family: var(--vscode-editor-font-family);
      font-size: 13px;
      white-space: pre-wrap;
      overflow: auto;
      max-height: calc(100vh - 180px);
      line-height: 1.5;
    }
    .actions { margin-top: 16px; }
    .actions button {
      background: var(--vscode-button-background);
      color: var(--vscode-button-foreground);
      border: none;
      padding: 6px 14px;
      border-radius: 4px;
      cursor: pointer;
      margin-right: 8px;
    }
    .cancel-btn {
      background: var(--vscode-errorForeground) !important;
    }
    .plan-section { margin-bottom: 12px; }
    .plan-section h4 { margin-bottom: 4px; }
    .progress-bar {
      height: 6px;
      background: var(--vscode-progressBar-background);
      border-radius: 3px;
      overflow: hidden;
      margin: 8px 0;
    }
    .progress-fill {
      height: 100%;
      background: var(--vscode-charts-green);
      transition: width 0.3s;
    }
  </style>
</head>
<body>
  <div class="header" id="header">
    <h2>&#x1F433; Loading...</h2>
  </div>
  <div class="tabs">
    <div class="tab active" onclick="switchTab('logs')">Logs</div>
    <div class="tab" onclick="switchTab('result')">Result</div>
    <div class="tab" onclick="switchTab('plan')">Plan</div>
  </div>
  <div class="content" id="content">Loading...</div>
  <div class="actions" id="actions"></div>

  <script>
    const vscode = acquireVsCodeApi();
    let currentTab = 'logs';
    let taskData = null;

    function switchTab(tab) {
      currentTab = tab;
      document.querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
      document.querySelectorAll('.tab')[['logs','result','plan'].indexOf(tab)].classList.add('active');
      document.getElementById('content').textContent = 'Loading...';
      vscode.postMessage({ type: 'switchTab', tab });
    }

    window.addEventListener('message', (event) => {
      const msg = event.data;
      switch (msg.type) {
        case 'taskData':
          taskData = msg.task;
          const phase = taskData.status?.phase || 'Pending';
          const started = taskData.status?.startTime ? new Date(taskData.status.startTime).toLocaleString() : 'N/A';
          document.getElementById('header').innerHTML =
            '<h2>&#x1F433; Task: ' + taskData.metadata.name + '</h2>' +
            '<div class="meta">' +
            '<span class="status-badge status-' + phase + '">' + phase + '</span> ' +
            'Type: ' + (taskData.spec.type || 'unknown') + ' \\u2022 ' +
            (taskData.spec.agentRef ? 'Agent: ' + taskData.spec.agentRef + ' \\u2022 ' : '') +
            'Started: ' + started +
            '</div>';
          const actionsEl = document.getElementById('actions');
          if (phase === 'Running' || phase === 'Pending') {
            actionsEl.innerHTML = '<button class="cancel-btn" onclick="cancelTask()">Cancel Task</button>' +
              '<button onclick="refresh()">Refresh</button>';
          } else {
            actionsEl.innerHTML = '<button onclick="refresh()">Refresh</button>';
          }
          // Auto-load current tab
          vscode.postMessage({ type: 'switchTab', tab: currentTab });
          break;
        case 'logsData':
          document.getElementById('content').textContent = msg.logs || 'No logs available';
          break;
        case 'resultData':
          document.getElementById('content').textContent = msg.result || 'No result available';
          break;
        case 'planData':
          if (msg.plan) {
            let html = '';
            if (msg.plan.Summary) html += '<div class="plan-section"><h4>Summary</h4><p>' + msg.plan.Summary + '</p></div>';
            if (msg.plan.ProgressPct !== undefined) {
              html += '<div class="plan-section"><h4>Progress: ' + msg.plan.ProgressPct + '%</h4>' +
                '<div class="progress-bar"><div class="progress-fill" style="width:' + msg.plan.ProgressPct + '%"></div></div></div>';
            }
            if (msg.plan.PlanDocument) html += '<div class="plan-section"><h4>Plan</h4><pre>' + msg.plan.PlanDocument + '</pre></div>';
            document.getElementById('content').innerHTML = html || 'No plan data';
          } else {
            document.getElementById('content').textContent = 'No plan available';
          }
          break;
        case 'error':
          document.getElementById('content').textContent = 'Error: ' + msg.message;
          break;
      }
    });

    function cancelTask() { vscode.postMessage({ type: 'cancelTask' }); }
    function refresh() { vscode.postMessage({ type: 'refresh' }); }

    // Initial load
    vscode.postMessage({ type: 'switchTab', tab: 'logs' });
  </script>
</body>
</html>`;
  }
}
