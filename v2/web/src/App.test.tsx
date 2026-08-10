import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import App, { DEFAULT_ORGANIZATION, DEFAULT_PROJECT } from './App';

async function connectToLive(user: ReturnType<typeof userEvent.setup>, token = 'owner-token-for-tests') {
  const input = await screen.findByLabelText('Owner token');
  await user.type(input, token);
  await user.click(screen.getByRole('button', { name: 'Connect securely' }));
}

describe('Agents Gateway console', () => {
  beforeEach(() => {
    window.location.hash = '';
    vi.stubEnv('VITE_AGW_MODE', 'demo');
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllEnvs();
  });

  it('renders the operational overview with explicit demo mode', async () => {
    render(<App />);

    expect(await screen.findByRole('heading', { name: 'Overview' })).toBeInTheDocument();
    expect(screen.getByText('Demo data')).toBeInTheDocument();
    expect(screen.getByRole('note', { name: 'Demo data notice' })).toHaveTextContent('not live operational status');
    expect(screen.queryByText('All systems operational')).not.toBeInTheDocument();
    expect(screen.getByText('System health')).toBeInTheDocument();
    expect(screen.getByText('98')).toBeInTheDocument();
  });

  it('renders the live overview from operational API collections', async () => {
    vi.stubEnv('VITE_AGW_MODE', 'live');
    vi.stubEnv('VITE_AGW_ORGANIZATION', 'org');
    vi.stubEnv('VITE_AGW_PROJECT', 'project');
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url.endsWith('/ready')) return new Response(JSON.stringify({ data: { status: 'ready' } }), { status: 200 });
      if (url.endsWith('/organizations/org/projects/project/runs')) return new Response(JSON.stringify({ data: { items: [{ id: 'live-run-1', kind: 'AgentRun', status: 'Running', definitionDigest: 'sha256:live', requestedBy: 'soham', createdAt: '2026-08-09T14:00:00Z', updatedAt: '2026-08-09T14:01:00Z' }, { id: 'live-run-2', kind: 'AgentRun', status: 'Failed', definitionDigest: 'sha256:live', requestedBy: 'soham', createdAt: '2026-08-09T13:00:00Z', updatedAt: '2026-08-09T13:02:00Z' }], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
      if (url.endsWith('/organizations/org/projects/project/runners')) return new Response(JSON.stringify({ data: { items: [{ kind: 'Runner', name: 'runner-live', digest: 'sha256:runner', revision: 1, document: { spec: { labels: { host: 'ovh' }, backends: ['rootless Podman'] }, status: { ready: true, isolation: 'standard' } } }], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
      if (url.endsWith('/organizations/org/projects/project/approvals')) return new Response(JSON.stringify({ data: { items: [{ kind: 'Approval', name: 'approval-live', digest: 'sha256:approval', revision: 1, document: { spec: { runRef: 'live-run-1', role: 'write' }, status: { decision: 'pending' } } }], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
      if (url.endsWith('/organizations/org/projects/project/usage')) return new Response(JSON.stringify({ data: { usage: { activeRuns: 1, totalRuns: 2, artifactVersions: 3, artifactBytes: 4096 } } }), { status: 200 });
      if (url.endsWith('/organizations/org/projects/project/audit')) return new Response(JSON.stringify({ data: { items: [{ principalId: 'soham', action: 'tool.denied', resourceType: 'run', resourceId: 'live-run-2', decision: 'denied', createdAt: '2026-08-09T14:02:00Z' }], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
      throw new Error(`unexpected request ${url}`);
    });
    const user = userEvent.setup();
    render(<App />);
    await connectToLive(user);

    expect(await screen.findAllByText('live-run-1')).not.toHaveLength(0);
    expect(screen.getByText('Active runs')).toBeInTheDocument();
    expect(screen.getByText('System readiness')).toBeInTheDocument();
    expect(screen.getByText('1 approval waiting')).toBeInTheDocument();
    expect(screen.getByText('1 denied audit decision')).toBeInTheDocument();
    expect(screen.queryByText('Demo healthy')).not.toBeInTheDocument();
    expect(screen.queryByText('Demo data')).not.toBeInTheDocument();
  });

  it('navigates to definitions and shows revision details', async () => {
    const user = userEvent.setup();
    render(<App />);

    await user.click(screen.getByRole('button', { name: /Definitions/ }));

    expect(await screen.findByRole('heading', { name: 'Definitions', level: 1 })).toBeInTheDocument();
    expect(screen.getAllByText('issue-fixer')).not.toHaveLength(0);
    expect(screen.getByText('Immutable applied revision')).toBeInTheDocument();
  });

  it('supports approval decisions and communicates an empty state', async () => {
    const user = userEvent.setup();
    render(<App />);
    await user.click(screen.getByRole('button', { name: /Approvals/ }));

    const approval = await screen.findByText('Create pull request');
    expect(approval).toBeInTheDocument();
    await user.click(screen.getAllByRole('button', { name: 'Approve' })[0]);

    expect(screen.queryByText('Create pull request')).not.toBeInTheDocument();
    expect(screen.queryByText('Nothing here yet')).not.toBeInTheDocument();
    expect(screen.getByText('2 pending decisions')).toBeInTheDocument();
  });

  it('keeps the console usable on narrow viewports with a navigation menu', async () => {
    const user = userEvent.setup();
    render(<App />);
    await user.click(screen.getByRole('button', { name: 'Open navigation' }));
    expect(screen.getByRole('button', { name: 'Close navigation' })).toBeInTheDocument();
  });

  it('loads definitions from the authenticated definitions collection instead of demo rows', async () => {
    vi.stubEnv('VITE_AGW_MODE', 'live');
    vi.stubEnv('VITE_AGW_ORGANIZATION', 'org');
    vi.stubEnv('VITE_AGW_PROJECT', 'project');
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url.endsWith('/ready')) return new Response(JSON.stringify({ data: { status: 'ready' } }), { status: 200 });
      expect(url).toBe('/api/v1alpha1/organizations/org/projects/project/definitions');
      return new Response(JSON.stringify({ data: { items: [{ kind: 'Agent', name: 'live-agent', digest: 'sha256:live', revision: 7, createdAt: '2026-08-08T14:00:00Z', document: { metadata: { name: 'live-agent' }, spec: { runtime: { harness: 'codex' }, skills: [], toolSetRef: 'tools' } } }], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
    });
    const user = userEvent.setup();
    render(<App />);
    await connectToLive(user);

    await user.click(screen.getByRole('button', { name: /Definitions/ }));

    expect(await screen.findAllByText('live-agent')).not.toHaveLength(0);
    expect(screen.queryByText('issue-fixer')).not.toBeInTheDocument();
    expect(screen.queryByText('Fallback demo data')).not.toBeInTheDocument();
  });

  it('loads the runs collection and its live event envelope', async () => {
    vi.stubEnv('VITE_AGW_MODE', 'live');
    vi.stubEnv('VITE_AGW_ORGANIZATION', 'Astatide');
    vi.stubEnv('VITE_AGW_PROJECT', 'agents-gateway');
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url.endsWith('/ready')) return new Response(JSON.stringify({ data: { status: 'ready' } }), { status: 200 });
      if (url.endsWith('/runs/run-live-1/events?after=0')) return new Response(JSON.stringify({ data: { events: [{ sequence: 1, type: 'run.started', payload: { owner: 'soham' }, createdAt: '2026-08-08T14:00:00Z' }] } }), { status: 200 });
      if (url.endsWith('/organizations/Astatide/projects/agents-gateway/runs')) return new Response(JSON.stringify({ data: { items: [{ id: 'run-live-1', kind: 'AgentRun', status: 'Running', definitionDigest: 'sha256:definition', requestedBy: 'soham', createdAt: '2026-08-08T14:00:00Z', updatedAt: '2026-08-08T14:01:00Z' }], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
      throw new Error(`unexpected request ${url}`);
    });
    const user = userEvent.setup();
    render(<App />);
    await connectToLive(user);

    await user.click(screen.getByRole('button', { name: /Runs & workflows/ }));

    expect(await screen.findAllByText('run-live-1')).not.toHaveLength(0);
    expect(await screen.findByText('run.started')).toBeInTheDocument();
    expect(screen.queryByText('Demo execution')).not.toBeInTheDocument();
  });

  it('renders the primary run artifact and redacted live activity details', async () => {
    vi.stubEnv('VITE_AGW_MODE', 'live');
    vi.stubEnv('VITE_AGW_ORGANIZATION', 'org');
    vi.stubEnv('VITE_AGW_PROJECT', 'project');
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url.endsWith('/ready')) return new Response(JSON.stringify({ data: { status: 'ready' } }), { status: 200 });
      if (url.endsWith('/organizations/org/projects/project/runs')) return new Response(JSON.stringify({ data: { items: [{ id: 'run-output-1', kind: 'AgentRun', status: 'Succeeded', definitionDigest: 'sha256:definition', requestedBy: 'soham', createdAt: '2026-08-09T14:00:00Z', updatedAt: '2026-08-09T14:01:00Z' }], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
      if (url.endsWith('/organizations/org/projects/project/runs/run-output-1/artifacts')) return new Response(JSON.stringify({ data: {
        runId: 'run-output-1',
        primary: { artifact_id: 'artifact-report', version_id: 'report-v1', outputRole: 'primary', created_at: '2026-08-09T14:01:00Z', manifest: { title: 'Acceptance report', description: 'The completed report.', content_kind: 'document', media_type: 'text/markdown', security: { allow: [] } }, content: { digest: 'sha256:report', size_bytes: 23, media_type: 'text/markdown' } },
        supporting: [{ artifact_id: 'artifact-log', version_id: 'log-v1', outputRole: 'supporting', created_at: '2026-08-09T14:01:00Z', manifest: { title: 'Execution log', content_kind: 'document', media_type: 'text/plain', security: { allow: [] } }, content: { digest: 'sha256:log', size_bytes: 5, media_type: 'text/plain' } }],
        artifacts: [
          { artifact_id: 'artifact-report', version_id: 'report-v1', outputRole: 'primary', created_at: '2026-08-09T14:01:00Z', manifest: { title: 'Acceptance report', description: 'The completed report.', content_kind: 'document', media_type: 'text/markdown', security: { allow: [] } }, content: { digest: 'sha256:report', size_bytes: 23, media_type: 'text/markdown' } },
          { artifact_id: 'artifact-log', version_id: 'log-v1', outputRole: 'supporting', created_at: '2026-08-09T14:01:00Z', manifest: { title: 'Execution log', content_kind: 'document', media_type: 'text/plain', security: { allow: [] } }, content: { digest: 'sha256:log', size_bytes: 5, media_type: 'text/plain' } },
        ],
      } }), { status: 200 });
      if (url.endsWith('/artifacts/artifact-report/versions/report-v1/content')) return new Response('# Finished report\n\nAll checks passed.', { status: 200, headers: { 'Content-Type': 'text/markdown', 'Content-Length': '35' } });
      if (url.endsWith('/organizations/org/projects/project/runs/run-output-1/events?after=0')) return new Response(JSON.stringify({ data: { events: [
        { sequence: 1, type: 'run.started', payload: {}, createdAt: '2026-08-09T14:00:00Z' },
        { sequence: 2, type: 'tool.completed', payload: { tool: 'github.get_me', arguments: { secret: 'must-not-render' }, status: 'ok' }, createdAt: '2026-08-09T14:00:10Z' },
      ] } }), { status: 200 });
      if (url.includes('/organizations/org/projects/project/runs/run-output-1/events?stream=1')) return new Response(new ReadableStream({ start(controller) { controller.enqueue(new TextEncoder().encode(': heartbeat\n\n')); controller.close(); } }), { status: 200 });
      return new Response(JSON.stringify({ data: { items: [], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
    });
    const user = userEvent.setup();
    render(<App />);
    await connectToLive(user);
    await user.click(screen.getByRole('button', { name: /Runs & workflows/ }));

    expect(await screen.findByRole('heading', { name: 'Acceptance report' })).toBeInTheDocument();
    expect(screen.getByText('Execution log')).toBeInTheDocument();
    expect(screen.getByText('tool.completed')).toBeInTheDocument();
    expect(screen.queryByText('must-not-render')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Tools' }));
    expect(screen.getByText('tool.completed')).toBeInTheDocument();
  });

  it('renders live audit entries from the authenticated audit collection', async () => {
    vi.stubEnv('VITE_AGW_MODE', 'live');
    vi.stubEnv('VITE_AGW_ORGANIZATION', 'Astatide');
    vi.stubEnv('VITE_AGW_PROJECT', 'agents-gateway');
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url.endsWith('/ready')) return new Response(JSON.stringify({ data: { status: 'ready' } }), { status: 200 });
      expect(url).toContain('/organizations/Astatide/projects/agents-gateway/audit');
      return new Response(JSON.stringify({ data: { items: [{ principalId: 'soham', action: 'run.created', resourceType: 'run', resourceId: 'run-live-1', decision: 'allowed', createdAt: '2026-08-08T14:00:00Z' }], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
    });
    const user = userEvent.setup();
    render(<App />);
    await connectToLive(user);

    await user.click(screen.getByRole('button', { name: /Audit history/ }));

    expect(await screen.findByText('run.created')).toBeInTheDocument();
    expect(screen.getByText('soham')).toBeInTheDocument();
    expect(screen.queryByText('approval.granted')).not.toBeInTheDocument();
  });

  it('renders a live empty state when a collection has no records', async () => {
    vi.stubEnv('VITE_AGW_MODE', 'live');
    vi.stubEnv('VITE_AGW_ORGANIZATION', 'Astatide');
    vi.stubEnv('VITE_AGW_PROJECT', 'agents-gateway');
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url.endsWith('/ready')) return new Response(JSON.stringify({ data: { status: 'ready' } }), { status: 200 });
      expect(url).toContain('/organizations/Astatide/projects/agents-gateway/runners');
      return new Response(JSON.stringify({ data: { items: [], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
    });
    const user = userEvent.setup();
    render(<App />);
    await connectToLive(user);

    await user.click(screen.getByRole('button', { name: /Runners/ }));

    expect(await screen.findByText('Nothing here yet')).toBeInTheDocument();
    expect(screen.getByText(/No runners have been enrolled/)).toBeInTheDocument();
    expect(screen.queryByText('runner-ovh-01')).not.toBeInTheDocument();
  });

  it('defaults to live mode and requires an explicit owner connection', () => {
    vi.unstubAllEnvs();
    vi.spyOn(globalThis, 'fetch').mockRejectedValue(new Error('offline'));

    render(<App />);

    expect(screen.getByRole('heading', { name: 'Connect to your gateway' })).toBeInTheDocument();
    expect(screen.getByText(DEFAULT_ORGANIZATION)).toBeInTheDocument();
    expect(screen.getByText(DEFAULT_PROJECT)).toBeInTheDocument();
    expect(screen.queryByText('Demo data')).not.toBeInTheDocument();
  });

  it('keeps the owner token in memory and authenticates collection requests', async () => {
    vi.unstubAllEnvs();
    const token = 'owner-token-never-persisted';
    const originalLocalStorage = Object.getOwnPropertyDescriptor(window, 'localStorage');
    const originalSessionStorage = Object.getOwnPropertyDescriptor(window, 'sessionStorage');
    Object.defineProperty(window, 'localStorage', { configurable: true, get: () => { throw new Error('localStorage must not be accessed'); } });
    Object.defineProperty(window, 'sessionStorage', { configurable: true, get: () => { throw new Error('sessionStorage must not be accessed'); } });
    const fetcher = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
      const url = String(input);
      if (url.endsWith('/ready')) return new Response(JSON.stringify({ data: { status: 'ready' } }), { status: 200 });
      expect(url).not.toContain(token);
      expect((init?.headers as Headers).get('Authorization')).toBe(`Bearer ${token}`);
      return new Response(JSON.stringify({ data: { items: [], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
    });

    try {
      const user = userEvent.setup();
      render(<App />);
      await connectToLive(user, token);
      expect(screen.queryByDisplayValue(token)).not.toBeInTheDocument();
      expect(document.body.textContent).not.toContain(token);
      expect(window.location.href).not.toContain(token);

      await user.click(screen.getByRole('button', { name: /Definitions/ }));
      await screen.findByText('Nothing here yet');
      expect(fetcher.mock.calls.some(([input]) => String(input).endsWith('/definitions'))).toBe(true);
    } finally {
      if (originalLocalStorage) Object.defineProperty(window, 'localStorage', originalLocalStorage);
      else delete (window as { localStorage?: unknown }).localStorage;
      if (originalSessionStorage) Object.defineProperty(window, 'sessionStorage', originalSessionStorage);
      else delete (window as { sessionStorage?: unknown }).sessionStorage;
    }
  });

  it('disconnects and clears the in-memory owner token', async () => {
    vi.unstubAllEnvs();
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
      if (String(input).endsWith('/ready')) return new Response(JSON.stringify({ data: { status: 'ready' } }), { status: 200 });
      expect((init?.headers as Headers).get('Authorization')).toBe('Bearer owner-token-for-disconnect');
      return new Response(JSON.stringify({ data: { items: [], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
    });
    const user = userEvent.setup();
    render(<App />);
    await connectToLive(user, 'owner-token-for-disconnect');
    await user.click(screen.getByRole('button', { name: 'Disconnect token' }));
    expect(await screen.findByRole('heading', { name: 'Connect to your gateway' })).toBeInTheDocument();
    expect(screen.queryByDisplayValue('owner-token-for-disconnect')).not.toBeInTheDocument();
  });
});
