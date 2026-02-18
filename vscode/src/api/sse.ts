import { EventEmitter } from 'events';

// === SSE Event Types ===

export interface SSEStatusEvent {
  type: 'status';
  sessionId: string;
  provider?: string;
  model?: string;
}

export interface SSEMessageEvent {
  type: 'message';
  content: string;
}

export interface SSEToolCallEvent {
  type: 'tool_call';
  id?: string;
  name: string;
  arguments: string;
}

export interface SSEToolResultEvent {
  type: 'tool_result';
  id?: string;
  name: string;
  result: string;
}

export interface SSEErrorEvent {
  type: 'error';
  message: string;
}

export interface SSEDoneEvent {
  type: 'done';
  usage?: {
    promptTokens?: number;
    completionTokens?: number;
    totalTokens?: number;
  };
}

export type SSEEvent = SSEStatusEvent | SSEMessageEvent | SSEToolCallEvent | SSEToolResultEvent | SSEErrorEvent | SSEDoneEvent;

// === Chat Request ===

export interface ChatRequest {
  message: string;
  sessionId?: string;
  provider?: string;
  model?: string;
  agentRef?: string;
  systemPrompt?: string;
  temperature?: number;
  maxTokens?: number;
}

// === SSE Client ===

export class OrkaSSEClient extends EventEmitter {
  private abortController: AbortController | null = null;

  /**
   * Start a chat SSE stream
   */
  async chat(baseUrl: string, authToken: string, request: ChatRequest): Promise<void> {
    this.abort(); // Cancel any existing stream

    this.abortController = new AbortController();
    const { signal } = this.abortController;

    const headers: Record<string, string> = {
      'Content-Type': 'application/json',
      'Accept': 'text/event-stream',
    };
    if (authToken) {
      headers['Authorization'] = `Bearer ${authToken}`;
    }

    try {
      const response = await fetch(`${baseUrl}/api/v1/chat`, {
        method: 'POST',
        headers,
        body: JSON.stringify(request),
        signal,
      });

      if (!response.ok) {
        const text = await response.text();
        this.emit('error', { type: 'error', message: `HTTP ${response.status}: ${text}` } as SSEErrorEvent);
        return;
      }

      if (!response.body) {
        this.emit('error', { type: 'error', message: 'No response body' } as SSEErrorEvent);
        return;
      }

      await this.processStream(response.body, signal);
    } catch (err: any) {
      if (err.name === 'AbortError') {
        return; // Expected when cancelled
      }
      this.emit('error', { type: 'error', message: err.message || 'Stream error' } as SSEErrorEvent);
    }
  }

  /**
   * Stream task logs via SSE
   */
  async streamTaskLogs(baseUrl: string, authToken: string, taskName: string, namespace: string): Promise<void> {
    this.abort();

    this.abortController = new AbortController();
    const { signal } = this.abortController;

    const headers: Record<string, string> = {
      'Accept': 'text/event-stream',
    };
    if (authToken) {
      headers['Authorization'] = `Bearer ${authToken}`;
    }

    try {
      const response = await fetch(
        `${baseUrl}/api/v1/tasks/${taskName}/logs?namespace=${namespace}&follow=true`,
        { method: 'GET', headers, signal }
      );

      if (!response.ok) {
        const text = await response.text();
        this.emit('error', { type: 'error', message: `HTTP ${response.status}: ${text}` } as SSEErrorEvent);
        return;
      }

      if (!response.body) {
        this.emit('error', { type: 'error', message: 'No response body' } as SSEErrorEvent);
        return;
      }

      await this.processStream(response.body, signal);
    } catch (err: any) {
      if (err.name === 'AbortError') {
        return;
      }
      this.emit('error', { type: 'error', message: err.message || 'Stream error' } as SSEErrorEvent);
    }
  }

  /**
   * Process an SSE stream from a ReadableStream
   */
  private async processStream(body: ReadableStream<Uint8Array>, signal: AbortSignal): Promise<void> {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';

    try {
      while (!signal.aborted) {
        const { done, value } = await reader.read();
        if (done) { break; }

        buffer += decoder.decode(value, { stream: true });

        // Process complete SSE messages (separated by double newline)
        const messages = buffer.split('\n\n');
        buffer = messages.pop() || '';

        for (const message of messages) {
          if (!message.trim()) { continue; }
          this.parseSSEMessage(message);
        }
      }
    } catch (err: any) {
      if (err.name !== 'AbortError') {
        this.emit('error', { type: 'error', message: err.message } as SSEErrorEvent);
      }
    } finally {
      reader.releaseLock();
    }
  }

  /**
   * Parse a single SSE message block
   */
  private parseSSEMessage(message: string): void {
    let eventType = 'message';
    let data = '';

    for (const line of message.split('\n')) {
      if (line.startsWith('event:')) {
        eventType = line.slice(6).trim();
      } else if (line.startsWith('data:')) {
        data += line.slice(5).trim();
      } else if (line.startsWith(':')) {
        // Comment, ignore
      }
    }

    if (!data) { return; }

    try {
      const parsed = JSON.parse(data);
      // Normalize error events: API sends {"error": "msg"} but we expect {"message": "msg"}
      if (eventType === 'error' && parsed.error && !parsed.message) {
        parsed.message = parsed.error;
      }
      const event: SSEEvent = { ...parsed, type: eventType };
      this.emit('event', event);
      this.emit(eventType, event);
    } catch {
      // Non-JSON data (e.g., plain text log lines)
      this.emit('event', { type: eventType, content: data } as SSEMessageEvent);
      this.emit(eventType, { type: eventType, content: data } as SSEMessageEvent);
    }
  }

  /**
   * Abort the current stream
   */
  abort(): void {
    if (this.abortController) {
      this.abortController.abort();
      this.abortController = null;
    }
  }

  /**
   * Check if currently streaming
   */
  get isStreaming(): boolean {
    return this.abortController !== null && !this.abortController.signal.aborted;
  }
}
