import * as vscode from 'vscode';
import { orkaApi } from '../api/client.js';

export class DashboardPanel {
  public static currentPanel: DashboardPanel | undefined;
  private readonly panel: vscode.WebviewPanel;
  private disposables: vscode.Disposable[] = [];
  private refreshTimer: ReturnType<typeof setInterval> | undefined;

  public static createOrShow(extensionUri: vscode.Uri): void {
    if (DashboardPanel.currentPanel) {
      DashboardPanel.currentPanel.panel.reveal();
      return;
    }

    const panel = vscode.window.createWebviewPanel(
      'orkaDashboard',
      'Orka Dashboard',
      vscode.ViewColumn.Active,
      { enableScripts: true, retainContextWhenHidden: true }
    );

    DashboardPanel.currentPanel = new DashboardPanel(panel, extensionUri);
  }

  private constructor(panel: vscode.WebviewPanel, _extensionUri: vscode.Uri) {
    this.panel = panel;
    this.panel.webview.html = this.getHtml();

    this.panel.webview.onDidReceiveMessage(
      async (message) => {
        if (message.type === 'refresh') this.loadData();
      },
      null,
      this.disposables
    );

    this.panel.onDidDispose(() => this.dispose());

    this.loadData();
    this.refreshTimer = setInterval(() => this.loadData(), 10000);
  }

  private async loadData(): Promise<void> {
    try {
      const [tasks, agents, sessions] = await Promise.all([
        orkaApi.listTasks(),
        orkaApi.listAgents(),
        orkaApi.listSessions(),
      ]);

      const running = tasks.filter(t => t.status?.phase === 'Running').length;
      const pending = tasks.filter(t => t.status?.phase === 'Pending').length;
      const succeeded = tasks.filter(t => t.status?.phase === 'Succeeded').length;
      const failed = tasks.filter(t => t.status?.phase === 'Failed').length;

      const today = new Date();
      today.setHours(0, 0, 0, 0);
      const completedToday = tasks.filter(t =>
        t.status?.phase === 'Succeeded' &&
        t.status?.completionTime &&
        new Date(t.status.completionTime) >= today
      ).length;

      const recentTasks = tasks
        .sort((a, b) => {
          const ta = a.metadata.creationTimestamp || '';
          const tb = b.metadata.creationTimestamp || '';
          return tb.localeCompare(ta);
        })
        .slice(0, 10);

      this.panel.webview.postMessage({
        type: 'dashboardData',
        data: {
          agentCount: agents.length,
          running,
          pending,
          succeeded,
          failed,
          completedToday,
          sessionCount: sessions.length,
          recentTasks: recentTasks.map(t => ({
            name: t.metadata.name,
            type: t.spec.type,
            phase: t.status?.phase || 'Pending',
            agent: t.spec.agentRef || '',
            created: t.metadata.creationTimestamp || '',
          })),
        },
      });
    } catch (err: any) {
      this.panel.webview.postMessage({ type: 'error', message: err.message });
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
      padding: 20px;
    }
    h1 { margin-bottom: 20px; }
    .stats {
      display: flex;
      gap: 16px;
      margin-bottom: 24px;
      flex-wrap: wrap;
    }
    .stat-card {
      background: var(--vscode-editor-inactiveSelectionBackground);
      border-radius: 8px;
      padding: 16px 24px;
      min-width: 140px;
      text-align: center;
    }
    .stat-card .value { font-size: 28px; font-weight: bold; }
    .stat-card .label { font-size: 12px; color: var(--vscode-descriptionForeground); margin-top: 4px; }
    h2 { margin: 16px 0 12px; }
    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 13px;
    }
    th, td {
      text-align: left;
      padding: 8px 12px;
      border-bottom: 1px solid var(--vscode-panel-border);
    }
    th { color: var(--vscode-descriptionForeground); font-weight: 600; }
    .phase { padding: 2px 8px; border-radius: 4px; font-size: 11px; font-weight: bold; }
    .phase-Running { background: var(--vscode-charts-yellow); color: #000; }
    .phase-Pending { background: var(--vscode-charts-blue); color: #fff; }
    .phase-Succeeded { background: var(--vscode-charts-green); color: #fff; }
    .phase-Failed { background: var(--vscode-charts-red); color: #fff; }
    .loading { color: var(--vscode-descriptionForeground); padding: 20px; text-align: center; }
  </style>
</head>
<body>
  <h1>&#x1F433; Orka Dashboard</h1>
  <div id="dashboard" class="loading">Loading...</div>

  <script>
    const vscode = acquireVsCodeApi();

    window.addEventListener('message', (event) => {
      const msg = event.data;
      if (msg.type === 'dashboardData') renderDashboard(msg.data);
      if (msg.type === 'error') {
        document.getElementById('dashboard').innerHTML = '<div class="loading">Error: ' + msg.message + '</div>';
      }
    });

    function renderDashboard(data) {
      let html = '<div class="stats">';
      html += statCard(data.agentCount, 'Agents');
      html += statCard(data.running, 'Running');
      html += statCard(data.pending, 'Pending');
      html += statCard(data.succeeded, 'Succeeded');
      html += statCard(data.failed, 'Failed');
      html += statCard(data.completedToday, 'Today');
      html += statCard(data.sessionCount, 'Sessions');
      html += '</div>';

      html += '<h2>Recent Activity</h2>';
      html += '<table><thead><tr><th>Name</th><th>Type</th><th>Agent</th><th>Status</th><th>Created</th></tr></thead><tbody>';
      data.recentTasks.forEach(t => {
        const created = t.created ? new Date(t.created).toLocaleString() : '';
        html += '<tr><td>' + t.name + '</td><td>' + t.type + '</td><td>' + (t.agent || '-') +
          '</td><td><span class="phase phase-' + t.phase + '">' + t.phase +
          '</span></td><td>' + created + '</td></tr>';
      });
      html += '</tbody></table>';

      document.getElementById('dashboard').innerHTML = html;
    }

    function statCard(value, label) {
      return '<div class="stat-card"><div class="value">' + value + '</div><div class="label">' + label + '</div></div>';
    }
  </script>
</body>
</html>`;
  }

  private dispose(): void {
    DashboardPanel.currentPanel = undefined;
    if (this.refreshTimer) clearInterval(this.refreshTimer);
    this.panel.dispose();
    this.disposables.forEach(d => d.dispose());
  }
}
