import * as vscode from 'vscode';

// === Type Definitions ===

export interface TaskSpec {
  type: 'container' | 'ai' | 'agent';
  // Container task fields
  image?: string;
  command?: string[];
  args?: string[];
  // AI/Agent task fields
  prompt?: string;
  agentRef?: string;
  provider?: string;
  model?: string;
  systemPrompt?: string;
  tools?: string[];
  autonomous?: boolean;
  maxIterations?: number;
}

export interface Task {
  metadata: {
    name: string;
    namespace?: string;
    creationTimestamp?: string;
    uid?: string;
    labels?: Record<string, string>;
  };
  spec: TaskSpec;
  status?: {
    phase?: string; // Pending, Running, Succeeded, Failed, Cancelled
    startTime?: string;
    completionTime?: string;
    result?: string;
    error?: string;
    llmCalls?: number;
    toolCalls?: number;
    totalTokens?: number;
  };
}

export interface Agent {
  metadata: {
    name: string;
    namespace?: string;
    creationTimestamp?: string;
    uid?: string;
  };
  spec: {
    provider?: string;
    model?: string;
    systemPrompt?: string;
    tools?: string[];
    maxTokens?: number;
    temperature?: number;
    fallbackProviders?: string[];
  };
}

export interface Tool {
  metadata: {
    name: string;
    namespace?: string;
  };
  spec: {
    description?: string;
    type?: string;
    parameters?: Record<string, any>;
    builtIn?: boolean;
  };
}

export interface Session {
  id: string;
  name?: string;
  namespace?: string;
  sessionType?: string;
  agentRef?: string;
  provider?: string;
  model?: string;
  messageCount?: number;
  inputTokens?: number;
  outputTokens?: number;
  totalTokens?: number;
  activeTask?: string;
  createdAt?: string;
  updatedAt?: string;
}

export interface ChatConfig {
  tools?: Tool[];
  providers?: string[];
  models?: string[];
}

export interface ListMetadata {
  continue?: string;
  remainingItemCount?: number | null;
}

export interface TaskListResponse {
  items: Task[];
  metadata?: ListMetadata;
}

export interface AgentListResponse {
  items: Agent[];
  metadata?: ListMetadata;
}

export interface ToolListResponse {
  items: Tool[];
  metadata?: ListMetadata;
}

export interface SessionListResponse {
  items: Session[];
  metadata?: ListMetadata;
}

export interface CreateTaskRequest {
  name: string;
  namespace?: string;
  spec: TaskSpec;
}

export interface CreateAgentRequest {
  name: string;
  namespace?: string;
  spec: Agent['spec'];
}

export interface TaskPlan {
  TaskName: string;
  Namespace: string;
  Iteration: number;
  Summary: string;
  ProgressPct: number;
  GoalComplete: boolean;
  PlanDocument: string;
  CreatedAt: string;
  UpdatedAt: string;
}

// === API Client Class ===

export class OrkaApiClient {
  private baseUrl: string = '';
  private authToken: string = '';
  private namespace: string = 'default';

  configure(baseUrl: string, authToken: string, namespace: string): void {
    this.baseUrl = baseUrl.replace(/\/+$/, '');
    this.authToken = authToken;
    this.namespace = namespace;
  }

  get isConfigured(): boolean {
    return this.baseUrl.length > 0;
  }

  private async request<T>(method: string, path: string, body?: any): Promise<T> {
    const headers: Record<string, string> = {
      'Content-Type': 'application/json',
    };
    if (this.authToken) {
      headers['Authorization'] = `Bearer ${this.authToken}`;
    }

    const url = `${this.baseUrl}${path}`;
    const response = await fetch(url, {
      method,
      headers,
      body: body ? JSON.stringify(body) : undefined,
    });

    if (!response.ok) {
      const text = await response.text();
      throw new Error(`API error ${response.status}: ${text}`);
    }

    if (response.status === 204) {
      return undefined as T;
    }

    return response.json() as Promise<T>;
  }

  // Auth
  async validateAuth(): Promise<boolean> {
    try {
      await this.request<any>('GET', '/api/v1/auth/validate');
      return true;
    } catch {
      return false;
    }
  }

  // Tasks
  async listTasks(): Promise<Task[]> {
    const resp = await this.request<TaskListResponse>('GET', `/api/v1/tasks?namespace=${this.namespace}`);
    return resp.items || [];
  }

  async getTask(name: string): Promise<Task> {
    return this.request<Task>('GET', `/api/v1/tasks/${name}?namespace=${this.namespace}`);
  }

  async createTask(req: CreateTaskRequest): Promise<Task> {
    return this.request<Task>('POST', '/api/v1/tasks', {
      ...req,
      namespace: req.namespace || this.namespace,
    });
  }

  async deleteTask(name: string): Promise<void> {
    await this.request<void>('DELETE', `/api/v1/tasks/${name}?namespace=${this.namespace}`);
  }

  async cancelTask(name: string): Promise<void> {
    await this.request<void>('DELETE', `/api/v1/tasks/${name}?namespace=${this.namespace}`);
  }

  async getTaskLogs(name: string): Promise<string> {
    const resp = await this.request<{ logs: string }>('GET', `/api/v1/tasks/${name}/logs?namespace=${this.namespace}`);
    return resp.logs || '';
  }

  async getTaskResult(name: string): Promise<string> {
    const resp = await this.request<{ result: string }>('GET', `/api/v1/tasks/${name}/result?namespace=${this.namespace}`);
    return resp.result || '';
  }

  async getTaskPlan(name: string): Promise<TaskPlan> {
    return this.request<TaskPlan>('GET', `/api/v1/tasks/${name}/plan?namespace=${this.namespace}`);
  }

  async getTaskChildren(name: string): Promise<Task[]> {
    const resp = await this.request<TaskListResponse>('GET', `/api/v1/tasks/${name}/children?namespace=${this.namespace}`);
    return resp.items || [];
  }

  // Agents
  async listAgents(): Promise<Agent[]> {
    const resp = await this.request<AgentListResponse>('GET', `/api/v1/agents?namespace=${this.namespace}`);
    return resp.items || [];
  }

  async getAgent(name: string): Promise<Agent> {
    return this.request<Agent>('GET', `/api/v1/agents/${name}?namespace=${this.namespace}`);
  }

  async createAgent(req: CreateAgentRequest): Promise<Agent> {
    return this.request<Agent>('POST', '/api/v1/agents', {
      ...req,
      namespace: req.namespace || this.namespace,
    });
  }

  async deleteAgent(name: string): Promise<void> {
    await this.request<void>('DELETE', `/api/v1/agents/${name}?namespace=${this.namespace}`);
  }

  // Tools
  async listTools(): Promise<Tool[]> {
    const resp = await this.request<ToolListResponse>('GET', `/api/v1/tools?namespace=${this.namespace}`);
    return resp.items || [];
  }

  async getTool(name: string): Promise<Tool> {
    return this.request<Tool>('GET', `/api/v1/tools/${name}?namespace=${this.namespace}`);
  }

  // Sessions
  async listSessions(): Promise<Session[]> {
    const resp = await this.request<SessionListResponse>('GET', `/api/v1/sessions?namespace=${this.namespace}`);
    return resp.items || [];
  }

  async getSession(id: string): Promise<Session> {
    return this.request<Session>('GET', `/api/v1/sessions/${id}?namespace=${this.namespace}`);
  }

  async deleteSession(id: string): Promise<void> {
    await this.request<void>('DELETE', `/api/v1/sessions/${id}?namespace=${this.namespace}`);
  }

  // Chat config
  async getChatConfig(): Promise<ChatConfig> {
    return this.request<ChatConfig>('GET', '/api/v1/chat/config');
  }

  // Connection info for SSE streaming
  getConnectionInfo(): { baseUrl: string; authToken: string; namespace: string } {
    return { baseUrl: this.baseUrl, authToken: this.authToken, namespace: this.namespace };
  }
}

// Singleton instance
export const orkaApi = new OrkaApiClient();
