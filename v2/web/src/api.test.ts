import { describe, expect, it, vi } from 'vitest';
import { ApiClient, ApiError } from './api';

function response(body: unknown, init: ResponseInit = {}) {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' }, ...init });
}

describe('ApiClient', () => {
  it('resolves bearer authentication at request time and unwraps data envelopes', async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(response({ data: { kind: 'Agent', name: 'issue-fixer', revision: 24 } }));
    const tokenProvider = vi.fn().mockResolvedValue('runtime-token');
    const client = new ApiClient({ baseUrl: 'https://gateway.test/api/v1alpha1', tokenProvider, fetcher });

    const resource = await client.getResource('astatide', 'agents-gateway', 'Agent', 'issue-fixer');

    expect(resource.name).toBe('issue-fixer');
    expect(fetcher).toHaveBeenCalledWith('https://gateway.test/api/v1alpha1/organizations/astatide/projects/agents-gateway/resources/Agent/issue-fixer', expect.objectContaining({ headers: expect.any(Headers) }));
    const headers = (fetcher.mock.calls[0]?.[1] as RequestInit).headers as Headers;
    expect(tokenProvider).toHaveBeenCalledTimes(1);
    expect(headers.get('Authorization')).toBe('Bearer runtime-token');
    expect(headers.get('Accept')).toBe('application/json');
    expect((fetcher.mock.calls[0]?.[1] as RequestInit).credentials).toBe('same-origin');
  });

  it('does not read credential values from build-time environment or browser storage', async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(response({ data: { status: 'ready' } }));
    const tokenProvider = vi.fn().mockReturnValue('must-not-be-sent-to-readiness');
    const buildCredentialKey = ['VITE', 'AGW', 'API', 'TOKEN'].join('_');
    const buildEnvironment = import.meta.env as Record<string, unknown>;
    buildEnvironment[buildCredentialKey] = 'must-not-be-used';
    const client = new ApiClient({ fetcher, tokenProvider });
    const originalStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
    Object.defineProperty(globalThis, 'localStorage', { configurable: true, get: () => { throw new Error('browser storage must not be accessed'); } });

    try {
      await client.health();
      await client.ready();
    } finally {
      if (originalStorage) Object.defineProperty(globalThis, 'localStorage', originalStorage);
      else delete (globalThis as { localStorage?: unknown }).localStorage;
    }

    expect(fetcher).toHaveBeenCalledTimes(2);
    for (const [, init] of fetcher.mock.calls) {
      const headers = (init as RequestInit).headers as Headers;
      expect(headers.get('Authorization')).toBeNull();
      expect((init as RequestInit).credentials).toBe('same-origin');
    }
    expect(tokenProvider).not.toHaveBeenCalled();
    delete buildEnvironment[buildCredentialKey];
  });

  it('serializes run creation with inputRef and surfaces API errors', async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(response({ error: { code: 'forbidden', message: 'not allowed' }, requestId: 'req-1' }, { status: 403 }));
    const client = new ApiClient({ fetcher });

    await expect(client.createRun('org', 'project', {
      kind: 'AgentRun',
      definitionDigest: 'sha256:abc',
      agentRef: 'issue-fixer',
      inputRef: 'artifacts://inputs/issue-842.json',
    })).rejects.toMatchObject({ status: 403, code: 'forbidden', requestId: 'req-1' });

    const request = fetcher.mock.calls[0]?.[1] as RequestInit;
    const body = JSON.parse(String(request.body)) as Record<string, unknown>;
    expect(body).toMatchObject({ agentRef: 'issue-fixer', inputRef: 'artifacts://inputs/issue-842.json' });
    expect(body).not.toHaveProperty('input');
  });

  it('submits an authenticated idempotent approval signal', async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(response({ data: { signal: 'approval', target: 'workflow' } }, { status: 202 }));
    const client = new ApiClient({ baseUrl: 'https://gateway.test/api/v1alpha1', tokenProvider: () => 'runtime-token', fetcher });

    await client.submitApproval('org', 'project', 'run-1', { target: 'workflow', approvalId: 'approval-1', stepId: 'publish', decision: 'approved' }, 'console-approval-1');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('https://gateway.test/api/v1alpha1/organizations/org/projects/project/runs/run-1/approval');
    expect((init?.headers as Headers).get('Authorization')).toBe('Bearer runtime-token');
    expect((init?.headers as Headers).get('Idempotency-Key')).toBe('console-approval-1');
    expect(JSON.parse(String(init?.body))).toMatchObject({ approvalId: 'approval-1', decision: 'approved' });
  });

  it('parses bearer-authenticated SSE frames and can be cancelled', async () => {
    const stream = new ReadableStream({
      start(controller) {
        controller.enqueue(new TextEncoder().encode('id: 8\nevent: tool.completed\ndata: {"sequence":8,"type":"tool.completed","payload":{},"createdAt":"2026-08-08T14:00:00Z"}\n\n'));
        controller.close();
      },
    });
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(new Response(stream, { status: 200 }));
    const listener = vi.fn();
    const client = new ApiClient({ tokenProvider: () => 'runtime-token', fetcher });
    const stop = client.subscribeToEvents('org', 'project', 'run-1', listener);
    await vi.waitFor(() => expect(listener).toHaveBeenCalledWith(expect.objectContaining({ sequence: 8, type: 'tool.completed' })));
    stop();
    expect((fetcher.mock.calls[0]?.[1] as RequestInit).headers).toEqual(expect.any(Headers));
    expect((fetcher.mock.calls[0]?.[1] as RequestInit).credentials).toBe('same-origin');
  });

  it('unwraps the event collection envelope used by the HTTP API', async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(response({ data: { events: [{ sequence: 3, type: 'run.started', payload: {}, createdAt: '2026-08-08T14:00:00Z' }] } }));
    const client = new ApiClient({ fetcher });

    await expect(client.listEvents('org', 'project', 'run-1')).resolves.toEqual([expect.objectContaining({ sequence: 3, type: 'run.started' })]);
  });

  it('loads authenticated tenant collections without configured resource references', async () => {
    const fetcher = vi.fn<typeof fetch>().mockImplementation(async () => response({ data: { items: [{ kind: 'Agent', name: 'live-agent' }], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }));
    const client = new ApiClient({ baseUrl: 'https://gateway.test/api/v1alpha1', tokenProvider: () => 'runtime-token', fetcher });

    await expect(client.listDefinitions('org', 'project')).resolves.toEqual([{ kind: 'Agent', name: 'live-agent' }]);
    await expect(client.listRuns('org', 'project')).resolves.toEqual([{ kind: 'Agent', name: 'live-agent' }]);
    await expect(client.listApprovals('org', 'project')).resolves.toEqual([{ kind: 'Agent', name: 'live-agent' }]);
    await expect(client.listRunners('org', 'project')).resolves.toEqual([{ kind: 'Agent', name: 'live-agent' }]);
    await expect(client.listEntitlements('org', 'project')).resolves.toEqual([{ kind: 'Agent', name: 'live-agent' }]);
    await expect(client.listModelRoutes('org', 'project')).resolves.toEqual([{ kind: 'Agent', name: 'live-agent' }]);
    await expect(client.listAudit('org', 'project')).resolves.toEqual([{ kind: 'Agent', name: 'live-agent' }]);

    const paths = fetcher.mock.calls.map(([url]) => String(url));
    expect(paths).toEqual([
      'https://gateway.test/api/v1alpha1/organizations/org/projects/project/definitions',
      'https://gateway.test/api/v1alpha1/organizations/org/projects/project/runs',
      'https://gateway.test/api/v1alpha1/organizations/org/projects/project/approvals',
      'https://gateway.test/api/v1alpha1/organizations/org/projects/project/runners',
      'https://gateway.test/api/v1alpha1/organizations/org/projects/project/entitlements',
      'https://gateway.test/api/v1alpha1/organizations/org/projects/project/model-routes',
      'https://gateway.test/api/v1alpha1/organizations/org/projects/project/audit',
    ]);
  });

  it('loads quota and usage envelopes from their dedicated endpoints', async () => {
    const fetcher = vi.fn<typeof fetch>()
      .mockResolvedValueOnce(response({ data: { quotas: { concurrentRuns: 4 }, usage: { activeRuns: 1, totalRuns: 2, artifactVersions: 3, artifactBytes: 4096 } } }))
      .mockResolvedValueOnce(response({ data: { usage: { activeRuns: 1, totalRuns: 2, artifactVersions: 3, artifactBytes: 4096 } } }));
    const client = new ApiClient({ baseUrl: 'https://gateway.test/api/v1alpha1', fetcher });

    await expect(client.getQuotas('org', 'project')).resolves.toMatchObject({ quotas: { concurrentRuns: 4 } });
    await expect(client.getUsage('org', 'project')).resolves.toMatchObject({ activeRuns: 1, artifactBytes: 4096 });
    expect(fetcher.mock.calls.map(([url]) => String(url))).toEqual([
      'https://gateway.test/api/v1alpha1/organizations/org/projects/project/quotas',
      'https://gateway.test/api/v1alpha1/organizations/org/projects/project/usage',
    ]);
  });

  it('preserves typed ApiError information for readiness failures', async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(response({ error: { code: 'unauthorized', message: 'token required' } }, { status: 401 }));
    const client = new ApiClient({ baseUrl: '/api/v1alpha1', fetcher });

    await expect(client.ready()).rejects.toEqual(expect.objectContaining({ status: 401, code: 'unauthorized' } satisfies Partial<ApiError>));
  });

  it('fetches artifact metadata and authenticated immutable content without exposing a token in the URL', async () => {
    const fetcher = vi.fn<typeof fetch>()
      .mockResolvedValueOnce(response({ data: [] }))
      .mockResolvedValueOnce(new Response('artifact body', { status: 200, headers: { 'Content-Type': 'text/plain', 'Content-Length': '13' } }));
    const client = new ApiClient({ baseUrl: 'https://gateway.test/api/v1alpha1', tokenProvider: () => 'runtime-token', fetcher });

    await client.listArtifacts('org', 'project');
    const content = await client.getArtifactContent('org', 'project', 'artifact-1', 'version-1');

    expect(content).toEqual({ body: 'artifact body', mediaType: 'text/plain', sizeBytes: 13 });
    const [url, init] = fetcher.mock.calls[1];
    expect(url).toBe('https://gateway.test/api/v1alpha1/organizations/org/projects/project/artifacts/artifact-1/versions/version-1/content');
    expect(String(url)).not.toContain('runtime-token');
    expect((init?.headers as Headers).get('Authorization')).toBe('Bearer runtime-token');
    expect(init?.credentials).toBe('same-origin');
  });
});
