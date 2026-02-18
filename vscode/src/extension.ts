import * as vscode from 'vscode';
import { AgentTreeProvider } from './views/agentTree';
import { TaskTreeProvider } from './views/taskTree';
import { ToolTreeProvider } from './views/toolTree';
import { SessionTreeProvider } from './views/sessionTree';
import { createStatusBarItem, onRefresh, connect, disconnect, disposeConnection } from './commands/connection';
import { createTask, cancelTask, deleteTask, runWithOrka } from './commands/tasks';
import { createAgent, deleteAgent } from './commands/agents';
import { ChatPanel } from './panels/chatPanel';
import { DashboardPanel } from './panels/dashboardPanel';
import { TaskDetailPanel } from './panels/taskDetailPanel';
import { getConnectionSettings, onConfigChange } from './utils/config';

export function activate(context: vscode.ExtensionContext) {
	console.log('Orka extension is now active');

	const extensionUri = context.extensionUri;

	// Status bar
	const statusBar = createStatusBarItem();
	context.subscriptions.push(statusBar);

	// Tree views
	const agentTreeProvider = new AgentTreeProvider();
	const taskTreeProvider = new TaskTreeProvider();
	const toolTreeProvider = new ToolTreeProvider();
	const sessionTreeProvider = new SessionTreeProvider();

	context.subscriptions.push(
		vscode.window.registerTreeDataProvider('orkaAgents', agentTreeProvider),
		vscode.window.registerTreeDataProvider('orkaTasks', taskTreeProvider),
		vscode.window.registerTreeDataProvider('orkaTools', toolTreeProvider),
		vscode.window.registerTreeDataProvider('orkaSessions', sessionTreeProvider),
	);

	// Refresh all tree views on connection changes
	onRefresh(() => {
		agentTreeProvider.refresh();
		taskTreeProvider.refresh();
		toolTreeProvider.refresh();
		sessionTreeProvider.refresh();
	});

	// Commands
	context.subscriptions.push(
		vscode.commands.registerCommand('orka.connect', () => connect()),
		vscode.commands.registerCommand('orka.disconnect', () => disconnect()),
		vscode.commands.registerCommand('orka.refreshAgents', () => agentTreeProvider.refresh()),
		vscode.commands.registerCommand('orka.refreshTasks', () => taskTreeProvider.refresh()),
		vscode.commands.registerCommand('orka.refreshTools', () => toolTreeProvider.refresh()),
		vscode.commands.registerCommand('orka.createTask', () => createTask()),
		vscode.commands.registerCommand('orka.cancelTask', (node?: { label?: string }) => cancelTask(node?.label ? String(node.label) : undefined)),
		vscode.commands.registerCommand('orka.deleteTask', (node?: { label?: string }) => deleteTask(node?.label ? String(node.label) : undefined)),
		vscode.commands.registerCommand('orka.createAgent', () => createAgent()),
		vscode.commands.registerCommand('orka.deleteAgent', (node?: { label?: string }) => deleteAgent(node?.label ? String(node.label) : undefined)),
		vscode.commands.registerCommand('orka.openChat', (sessionId?: string) => ChatPanel.createOrShow(extensionUri, sessionId)),
		vscode.commands.registerCommand('orka.openDashboard', () => DashboardPanel.createOrShow(extensionUri)),
		vscode.commands.registerCommand('orka.viewTaskDetail', (taskName: string) => TaskDetailPanel.createOrShow(extensionUri, taskName)),
		vscode.commands.registerCommand('orka.runWithOrka', () => runWithOrka()),
	);

	// Config change listener
	context.subscriptions.push(
		onConfigChange(() => {
			// Reconnect on config changes if needed
		}),
	);

	// Auto-connect
	const settings = getConnectionSettings();
	if (settings.autoConnect) {
		connect();
	}
}

export function deactivate() {
	disposeConnection();
}
