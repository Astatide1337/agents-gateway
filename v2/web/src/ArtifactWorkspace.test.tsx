import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { ArtifactWorkspace, type ArtifactSummary } from './ArtifactWorkspace';

const artifact: ArtifactSummary = {
  id: 'artifact-report',
  title: 'Verification report',
  description: 'Immutable output from a completed run.',
  contentKind: 'code',
  mediaType: 'text/plain',
  currentVersionId: 'v2',
  versions: [
    { id: 'v1', version: 1, createdAt: '2026-08-08T12:00:00Z', digest: 'sha256:old' },
    { id: 'v2', version: 2, createdAt: '2026-08-08T13:00:00Z', digest: 'sha256:new' },
  ],
  lineage: { relation: 'fork', parentArtifactId: 'artifact-run-output', parentTitle: 'Run output' },
};

describe('ArtifactWorkspace', () => {
  it('renders source as escaped text instead of HTML', async () => {
    const user = userEvent.setup();
    const { container } = render(<ArtifactWorkspace artifact={artifact} content={{ source: '<script>alert("owned")</script><b>not markup</b>' }} />);

    await user.click(screen.getByRole('tab', { name: 'Source' }));

    expect(screen.getByRole('tabpanel')).toHaveTextContent('<script>alert("owned")</script><b>not markup</b>');
    expect(container.querySelector('script')).toBeNull();
    expect(container.querySelector('b')).toBeNull();
  });

  it('selects immutable versions and notifies the caller', async () => {
    const user = userEvent.setup();
    const onVersionChange = vi.fn();
    render(<ArtifactWorkspace artifact={artifact} content="version content" onVersionChange={onVersionChange} />);

    await user.selectOptions(screen.getByRole('combobox', { name: 'Artifact version' }), 'v1');

    expect(onVersionChange).toHaveBeenCalledWith(expect.objectContaining({ id: 'v1', version: 1 }));
    expect(screen.getByText('sha256:old')).toBeInTheDocument();
  });

  it('calls the provided download action for the selected version', async () => {
    const user = userEvent.setup();
    const onDownload = vi.fn();
    render(<ArtifactWorkspace artifact={artifact} content="download me" onDownload={onDownload} />);

    await user.click(screen.getByRole('button', { name: 'Download' }));

    expect(onDownload).toHaveBeenCalledWith(expect.objectContaining({ id: 'v2', version: 2 }));
  });

  it('uses a restrictive sandbox and CSP for interactive previews', () => {
    const interactive: ArtifactSummary = { ...artifact, id: 'artifact-ui', title: 'Interactive UI', contentKind: 'interactive_component', versions: [{ ...artifact.versions[1], contentKind: 'interactive_component', capabilities: ['sandboxed_scripts'] }] };
    render(<ArtifactWorkspace artifact={interactive} content={{ source: '<button onclick="alert(1)">Run</button>', preview: '<button>Preview</button>' }} />);

    const frame = screen.getByTitle('Interactive UI preview');
    const sandbox = frame.getAttribute('sandbox') ?? '';
    const srcDoc = frame.getAttribute('srcdoc') ?? (frame as HTMLIFrameElement).srcdoc;

    expect(sandbox).toContain('allow-scripts');
    expect(sandbox).not.toContain('allow-same-origin');
    expect(srcDoc).toContain("default-src 'none'");
    expect(srcDoc).toContain("connect-src 'none'");
    expect(srcDoc).toContain("frame-src 'none'");
    expect(srcDoc).toContain("script-src 'unsafe-inline'");
  });

  it('keeps interactive scripts disabled unless the contract grants them', () => {
    const interactive: ArtifactSummary = { ...artifact, id: 'artifact-inert', title: 'Inert UI', contentKind: 'interactive_component', versions: [{ ...artifact.versions[1], contentKind: 'interactive_component', capabilities: [] }] };
    render(<ArtifactWorkspace artifact={interactive} content={{ source: '<script>window.owned=true</script>' }} />);

    const frame = screen.getByTitle('Inert UI preview');
    expect(frame.getAttribute('sandbox')).toBe('');
    expect((frame as HTMLIFrameElement).srcdoc).toContain("script-src 'none'");
  });

  it('renders Markdown while keeping raw HTML and network links inert', () => {
    const markdown: ArtifactSummary = {
      ...artifact,
      id: 'artifact-markdown',
      title: 'Review summary',
      contentKind: 'document',
      mediaType: 'text/markdown',
      versions: [{ ...artifact.versions[1], contentKind: 'document', mediaType: 'text/markdown' }],
    };
    const { container } = render(
      <ArtifactWorkspace artifact={markdown} content={'# Safe heading\n\n[External](https://example.com)\n\n<script>window.owned=true</script>'} />,
    );

    expect(screen.getByRole('heading', { name: 'Safe heading' })).toBeInTheDocument();
    expect(screen.getByText('External')).not.toHaveAttribute('href');
    expect(container.querySelector('script')).toBeNull();
    expect(screen.queryByText('window.owned=true')).not.toBeInTheDocument();
  });
});
