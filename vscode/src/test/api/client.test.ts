import { describe, it, beforeEach, afterEach, mock } from 'node:test';
import assert from 'node:assert';
import { OrkaApiClient } from '../../api/client.js';
import type { CreateTaskRequest, CreateAgentRequest } from '../../api/client.js';

// Mock fetch helper
function createMockFetch(status: number, body: any, statusText = 'OK') {
  return mock.fn(async (_url: string | URL | Request, _init?: RequestInit) => ({
    ok: status >= 200 && status < 300,
    status,
    statusText,
    json: async () => body,
    text: async () => (typeof body === 'string' ? body : JSON.stringify(body)),
  } as unknown as Response));
}

describe('OrkaApiClient', () => {
  let client: OrkaApiClient;
  let originalFetch: typeof globalThis.fetch;

  beforeEach(() => {
    client = new OrkaApiClient();
    originalFetch = globalThis.fetch;
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  describe('configure()', () => {
    it('should set connection parameters', () => {
      client.configure('http://localhost:8080', 'test-token', 'my-ns');
      assert.strictEqual(client.isConfigured, true);
    });

    it('should strip trailing slashes from baseUrl', () => {
      client.configure('http://localhost:8080///', 'token', 'default');
      const info = client.getConnectionInfo();
      assert.strictEqual(info.baseUrl, 'http://localhost:8080');
    });

    it('should store namespace and token', () => {
      client.configure('http://host:9090', 'my-token', 'prod');
      const info = client.getConnectionInfo();
      assert.strictEqual(info.authToken, 'my-token');
      assert.strictEqual(info.namespace, 'prod');
    });
  });

  describe('isConfigured', () => {
    it('should return false when not configured', () => {
      assert.strictEqual(client.isConfigured, false);
    });

    it('should return true after configure is called', () => {
      client.configure('http://localhost:8080', '', 'default');
      assert.strictEqual(client.isConfigured, true);
    });
  });

  describe('listTasks()', () => {
    it('should call the correct URL and return tasks', async () => {
      const tasks = [
        { metadata: { name: 'task-1' }, spec: { type: 'ai', prompt: 'hello' } },
        { metadata: { name: 'task-2' }, spec: { type: 'container', image: 'nginx' } },
      ];
      const mockFetch = createMockFetch(200, { items: tasks });
      globalThis.fetch = mockFetch;
      client.configure('http://localhost:8080', 'token', 'default');

      const result = await client.listTasks();

      assert.strictEqual(result.length, 2);
      assert.strictEqual(result[0].metadata.name, 'task-1');
      assert.strictEqual(mockFetch.mock.calls.length, 1);
      const calledUrl = mockFetch.mock.calls[0].arguments[0] as string;
      assert.strictEqual(calledUrl, 'http://localhost:8080/api/v1/tasks?namespace=default');
    });

    it('should return empty array when items is null', async () => {
      globalThis.fetch = createMockFetch(200, { items: null });
      client.configure('http://localhost:8080', '', 'default');

      const result = await client.listTasks();
      assert.deepStrictEqual(result, []);
    });

    it('should include Authorization header when token is set', async () => {
      const mockFetch = createMockFetch(200, { items: [] });
      globalThis.fetch = mockFetch;
      client.configure('http://localhost:8080', 'my-secret-token', 'default');

      await client.listTasks();

      const init = mockFetch.mock.calls[0].arguments[1] as RequestInit;
      const headers = init.headers as Record<string, string>;
      assert.strictEqual(headers['Authorization'], 'Bearer my-secret-token');
    });

    it('should not include Authorization header when token is empty', async () => {
      const mockFetch = createMockFetch(200, { items: [] });
      globalThis.fetch = mockFetch;
      client.configure('http://localhost:8080', '', 'default');

      await client.listTasks();

      const init = mockFetch.mock.calls[0].arguments[1] as RequestInit;
      const headers = init.headers as Record<string, string>;
      assert.strictEqual(headers['Authorization'], undefined);
    });
  });

  describe('listAgents()', () => {
    it('should call the correct URL and return agents', async () => {
      const agents = [
        { metadata: { name: 'agent-1' }, spec: { provider: 'openai', model: 'gpt-4' } },
      ];
      const mockFetch = createMockFetch(200, { items: agents });
      globalThis.fetch = mockFetch;
      client.configure('http://localhost:8080', '', 'my-ns');

      const result = await client.listAgents();

      assert.strictEqual(result.length, 1);
      assert.strictEqual(result[0].metadata.name, 'agent-1');
      const calledUrl = mockFetch.mock.calls[0].arguments[0] as string;
      assert.strictEqual(calledUrl, 'http://localhost:8080/api/v1/agents?namespace=my-ns');
    });

    it('should return empty array when items is missing', async () => {
      globalThis.fetch = createMockFetch(200, {});
      client.configure('http://localhost:8080', '', 'default');

      const result = await client.listAgents();
      assert.deepStrictEqual(result, []);
    });
  });

  describe('listTools()', () => {
    it('should call the correct URL and return tools', async () => {
      const tools = [
        { metadata: { name: 'web_search' }, spec: { description: 'Search the web', builtIn: true } },
      ];
      const mockFetch = createMockFetch(200, { items: tools });
      globalThis.fetch = mockFetch;
      client.configure('http://localhost:8080', '', 'default');

      const result = await client.listTools();

      assert.strictEqual(result.length, 1);
      assert.strictEqual(result[0].metadata.name, 'web_search');
      const calledUrl = mockFetch.mock.calls[0].arguments[0] as string;
      assert.strictEqual(calledUrl, 'http://localhost:8080/api/v1/tools?namespace=default');
    });
  });

  describe('createTask()', () => {
    it('should POST with correct body', async () => {
      const created = {
        metadata: { name: 'new-task' },
        spec: { type: 'ai', prompt: 'do something' },
      };
      const mockFetch = createMockFetch(200, created);
      globalThis.fetch = mockFetch;
      client.configure('http://localhost:8080', 'token', 'default');

      const req: CreateTaskRequest = {
        name: 'new-task',
        spec: { type: 'ai', prompt: 'do something' },
      };
      const result = await client.createTask(req);

      assert.strictEqual(result.metadata.name, 'new-task');
      const init = mockFetch.mock.calls[0].arguments[1] as RequestInit;
      assert.strictEqual(init.method, 'POST');
      const calledUrl = mockFetch.mock.calls[0].arguments[0] as string;
      assert.strictEqual(calledUrl, 'http://localhost:8080/api/v1/tasks');
      const body = JSON.parse(init.body as string);
      assert.strictEqual(body.name, 'new-task');
      assert.strictEqual(body.namespace, 'default');
      assert.strictEqual(body.spec.prompt, 'do something');
    });

    it('should use request namespace if provided', async () => {
      const mockFetch = createMockFetch(200, { metadata: { name: 't' }, spec: { type: 'ai' } });
      globalThis.fetch = mockFetch;
      client.configure('http://localhost:8080', '', 'default');

      await client.createTask({ name: 't', namespace: 'custom-ns', spec: { type: 'ai', prompt: 'x' } });

      const init = mockFetch.mock.calls[0].arguments[1] as RequestInit;
      const body = JSON.parse(init.body as string);
      assert.strictEqual(body.namespace, 'custom-ns');
    });
  });

  describe('deleteTask()', () => {
    it('should call DELETE with correct URL', async () => {
      const mockFetch = createMockFetch(204, undefined);
      globalThis.fetch = mockFetch;
      client.configure('http://localhost:8080', '', 'default');

      await client.deleteTask('my-task');

      const init = mockFetch.mock.calls[0].arguments[1] as RequestInit;
      assert.strictEqual(init.method, 'DELETE');
      const calledUrl = mockFetch.mock.calls[0].arguments[0] as string;
      assert.strictEqual(calledUrl, 'http://localhost:8080/api/v1/tasks/my-task?namespace=default');
    });
  });

  describe('validateAuth()', () => {
    it('should return true on 200 response', async () => {
      globalThis.fetch = createMockFetch(200, { valid: true });
      client.configure('http://localhost:8080', 'token', 'default');

      const result = await client.validateAuth();
      assert.strictEqual(result, true);
    });

    it('should return false on error response', async () => {
      globalThis.fetch = createMockFetch(401, 'Unauthorized');
      client.configure('http://localhost:8080', 'bad-token', 'default');

      const result = await client.validateAuth();
      assert.strictEqual(result, false);
    });

    it('should return false on network error', async () => {
      globalThis.fetch = mock.fn(async () => { throw new Error('network error'); }) as typeof fetch;
      client.configure('http://localhost:8080', '', 'default');

      const result = await client.validateAuth();
      assert.strictEqual(result, false);
    });
  });

  describe('error handling', () => {
    it('should throw on non-OK responses', async () => {
      globalThis.fetch = createMockFetch(500, 'Internal Server Error');
      client.configure('http://localhost:8080', '', 'default');

      await assert.rejects(
        () => client.listTasks(),
        (err: Error) => {
          assert.ok(err.message.includes('API error 500'));
          return true;
        },
      );
    });

    it('should throw on 404 responses', async () => {
      globalThis.fetch = createMockFetch(404, 'Not Found');
      client.configure('http://localhost:8080', '', 'default');

      await assert.rejects(
        () => client.getTask('nonexistent'),
        (err: Error) => {
          assert.ok(err.message.includes('404'));
          return true;
        },
      );
    });
  });

  describe('createAgent()', () => {
    it('should POST with correct body', async () => {
      const created = {
        metadata: { name: 'my-agent' },
        spec: { provider: 'openai', model: 'gpt-4' },
      };
      const mockFetch = createMockFetch(200, created);
      globalThis.fetch = mockFetch;
      client.configure('http://localhost:8080', '', 'default');

      const req: CreateAgentRequest = {
        name: 'my-agent',
        spec: { provider: 'openai', model: 'gpt-4' },
      };
      const result = await client.createAgent(req);

      assert.strictEqual(result.metadata.name, 'my-agent');
      const init = mockFetch.mock.calls[0].arguments[1] as RequestInit;
      assert.strictEqual(init.method, 'POST');
      const calledUrl = mockFetch.mock.calls[0].arguments[0] as string;
      assert.strictEqual(calledUrl, 'http://localhost:8080/api/v1/agents');
    });
  });

  describe('deleteAgent()', () => {
    it('should call DELETE with correct URL', async () => {
      const mockFetch = createMockFetch(204, undefined);
      globalThis.fetch = mockFetch;
      client.configure('http://localhost:8080', '', 'prod');

      await client.deleteAgent('old-agent');

      const init = mockFetch.mock.calls[0].arguments[1] as RequestInit;
      assert.strictEqual(init.method, 'DELETE');
      const calledUrl = mockFetch.mock.calls[0].arguments[0] as string;
      assert.strictEqual(calledUrl, 'http://localhost:8080/api/v1/agents/old-agent?namespace=prod');
    });
  });

  describe('getConnectionInfo()', () => {
    it('should return current connection parameters', () => {
      client.configure('http://example.com:9090', 'tok', 'staging');
      const info = client.getConnectionInfo();
      assert.strictEqual(info.baseUrl, 'http://example.com:9090');
      assert.strictEqual(info.authToken, 'tok');
      assert.strictEqual(info.namespace, 'staging');
    });
  });

  describe('listSessions()', () => {
    it('should call the correct URL and return sessions', async () => {
      const sessions = [
        { id: 'sess-1', name: 'test-session', messageCount: 5 },
      ];
      const mockFetch = createMockFetch(200, { items: sessions });
      globalThis.fetch = mockFetch;
      client.configure('http://localhost:8080', '', 'default');

      const result = await client.listSessions();

      assert.strictEqual(result.length, 1);
      assert.strictEqual(result[0].id, 'sess-1');
      const calledUrl = mockFetch.mock.calls[0].arguments[0] as string;
      assert.strictEqual(calledUrl, 'http://localhost:8080/api/v1/sessions?namespace=default');
    });
  });
});
