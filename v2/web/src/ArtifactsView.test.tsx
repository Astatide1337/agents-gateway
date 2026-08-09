import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ApiArtifactVersion, ApiClient } from './api';
import { ArtifactsView, groupArtifactContracts } from './ArtifactsView';

function contract(overrides: Partial<ApiArtifactVersion> = {}): ApiArtifactVersion {
  const artifactID = overrides.artifact_id ?? 'artifact-1';
  const versionID = overrides.version_id ?? 'version-1';
  return {
    schema_version: 1,
    artifact_id: artifactID,
    version_id: versionID,
    created_at: overrides.created_at ?? '2026-08-09T15:00:00Z',
    lineage: overrides.lineage ?? {},
    manifest: overrides.manifest ?? {
      schema_version: 1,
      artifact_id: artifactID,
      title: 'Live report',
      description: 'A catalog artifact.',
      content_kind: 'document',
      media_type: 'text/plain',
      renderer: { kind: 'plain_text', source_view: true, download: true },
      security: {},
      preview: {},
    },
    content: overrides.content ?? { id: versionID, uri: `artifact://catalog/${artifactID}/${versionID}`, digest: `sha256:${'a'.repeat(64)}`, size_bytes: 12, media_type: 'text/plain' },
    source: overrides.source ?? { id: versionID, uri: `artifact://catalog/${artifactID}/${versionID}`, digest: `sha256:${'a'.repeat(64)}`, size_bytes: 12, media_type: 'text/plain' },
  };
}

describe('ArtifactsView', () => {
  it('groups immutable contracts into newest-first Claude-style versions', () => {
    const groups = groupArtifactContracts([
      contract({ version_id: 'version-1', created_at: '2026-08-09T14:00:00Z' }),
      contract({ version_id: 'version-2', created_at: '2026-08-09T15:00:00Z' }),
    ]);

    expect(groups).toHaveLength(1);
    expect(groups[0].summary.currentVersionId).toBe('version-2');
    expect(groups[0].summary.versions.map((version) => [version.id, version.version])).toEqual([['version-2', 2], ['version-1', 1]]);
  });

  it('loads live catalog metadata and authenticated content into the workspace', async () => {
    const api = {
      listArtifacts: vi.fn().mockResolvedValue([contract()]),
      getArtifactContent: vi.fn().mockResolvedValue({ body: '<b>literal source</b>', mediaType: 'text/plain', sizeBytes: 21 }),
      downloadArtifact: vi.fn(),
    } as unknown as ApiClient;

    render(<ArtifactsView api={api} liveMode organization="org" project="project" />);

    expect(await screen.findByRole('heading', { name: 'Live report' })).toBeInTheDocument();
    expect(await screen.findByLabelText('Text preview for Live report')).toHaveTextContent('<b>literal source</b>');
    expect(document.querySelector('.artifact-preview-frame')).toBeNull();
    expect(api.listArtifacts).toHaveBeenCalledWith('org', 'project');
    expect(api.getArtifactContent).toHaveBeenCalledWith('org', 'project', 'artifact-1', 'version-1');
  });
});
