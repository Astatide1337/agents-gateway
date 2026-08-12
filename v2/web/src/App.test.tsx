import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import App from './App';

async function connect(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByLabelText('Owner token'), 'owner-token');
  await user.click(screen.getByRole('button', { name: /Connect securely/i }));
}

describe('Agents Gateway live console', () => {
  it('requires an explicit in-memory owner connection', () => {
    vi.stubEnv('VITE_AGW_MODE', 'live');
    render(<App />);
    expect(screen.getByRole('heading', { name: 'Connect to your gateway' })).toBeInTheDocument();
    expect(screen.getByText(/never stores the token/i)).toBeInTheDocument();
  });

  it('renders a live project overview without fabricated records', async () => {
    vi.stubEnv('VITE_AGW_MODE', 'live');
    vi.stubEnv('VITE_AGW_ORGANIZATION', 'org');
    vi.stubEnv('VITE_AGW_PROJECT', 'project');
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url.endsWith('/ready')) return new Response(JSON.stringify({ data: { status: 'ready' } }), { status: 200 });
      if (url.endsWith('/runs')) return new Response(JSON.stringify({ data: { items: [{ id: 'run-live', kind: 'AgentRun', status: 'Running', definitionDigest: 'sha256:agent', requestedBy: 'owner', createdAt: '2026-08-10T00:00:00Z', updatedAt: '2026-08-10T00:01:00Z' }], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
      if (url.endsWith('/usage')) return new Response(JSON.stringify({ data: { usage: { activeRuns: 1, totalRuns: 1, artifactVersions: 0, artifactBytes: 0 } } }), { status: 200 });
      if (url.endsWith('/approvals')) return new Response(JSON.stringify({ data: { items: [], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
      if (url.endsWith('/resources')) return new Response(JSON.stringify({ data: { items: [{ kind: 'Agent', name: 'live-agent', digest: 'sha256:agent', revision: 1, document: { metadata: { name: 'live-agent' }, spec: {} } }], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
      return new Response(JSON.stringify({ data: { items: [], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
    });
    const user = userEvent.setup();
    render(<App />);
    await connect(user);
    expect(await screen.findByRole('heading', { name: 'Project overview' })).toBeInTheDocument();
    expect(screen.getByText('run-live')).toBeInTheDocument();
    expect(screen.queryByText('issue-fixer')).not.toBeInTheDocument();
  });

  it('opens an applied resource manifest from the resource browser', async () => {
    vi.stubEnv('VITE_AGW_MODE', 'live');
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url.endsWith('/ready')) return new Response(JSON.stringify({ data: { status: 'ready' } }), { status: 200 });
      if (url.includes('/resources?kind=Agent')) return new Response(JSON.stringify({ data: { items: [{ kind: 'Agent', name: 'agent-one', digest: 'sha256:one', revision: 3, document: { metadata: { name: 'agent-one' }, spec: { runtime: { harness: 'codex' } } } }], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
      if (url.endsWith('/resources')) return new Response(JSON.stringify({ data: { items: [], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
      return new Response(JSON.stringify({ data: { items: [], hasMore: false, nextOffset: null, limit: 100, offset: 0 } }), { status: 200 });
    });
    const user = userEvent.setup();
    render(<App />);
    await connect(user);
    await user.click(screen.getByRole('button', { name: 'Agents' }));
    await user.click(await screen.findByRole('button', { name: 'View' }));
    expect(screen.getByRole('dialog', { name: 'Agent agent-one' })).toBeInTheDocument();
    expect(screen.getByText(/Read-only applied resource/)).toBeInTheDocument();
    expect(screen.getByText(/"harness": "codex"/)).toBeInTheDocument();
  });
});
