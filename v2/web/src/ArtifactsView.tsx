import { useEffect, useMemo, useState } from 'react';
import { Code2, FileText, GitBranch, Package } from 'lucide-react';
import type { ApiArtifactVersion, ApiClient } from './api';
import ArtifactWorkspace, {
  type ArtifactContent,
  type ArtifactContentStatus,
  type ArtifactSummary,
  type ArtifactVersion,
} from './ArtifactWorkspace';

interface ArtifactsViewProps {
  api: ApiClient;
  liveMode: boolean;
  organization: string;
  project: string;
}

interface ArtifactGroup {
  summary: ArtifactSummary;
  contracts: Map<string, ApiArtifactVersion>;
}

const demoArtifacts: ArtifactGroup[] = [
  {
    summary: {
      id: 'demo-release-notes',
      title: 'release-notes.html',
      description: 'A standalone release summary produced by the release agent.',
      contentKind: 'single_page_html',
      mediaType: 'text/html',
      currentVersionId: 'demo-release-v2',
      versions: [
        { id: 'demo-release-v2', version: 2, createdAt: '2026-08-09T15:14:00Z', digest: `sha256:${'a'.repeat(64)}`, sizeBytes: 1314, mediaType: 'text/html', contentKind: 'single_page_html' },
        { id: 'demo-release-v1', version: 1, createdAt: '2026-08-09T14:48:00Z', digest: `sha256:${'b'.repeat(64)}`, sizeBytes: 998, mediaType: 'text/html', contentKind: 'single_page_html' },
      ],
    },
    contracts: new Map(),
  },
  {
    summary: {
      id: 'demo-review-summary',
      title: 'review-summary.md',
      description: 'Verified findings and recommendations from a completed code review.',
      contentKind: 'document',
      mediaType: 'text/markdown',
      currentVersionId: 'demo-review-v1',
      versions: [{ id: 'demo-review-v1', version: 1, createdAt: '2026-08-09T13:20:00Z', digest: `sha256:${'c'.repeat(64)}`, sizeBytes: 612, mediaType: 'text/markdown', contentKind: 'document' }],
      lineage: { relation: 'remix', parentArtifactId: 'demo-run-output', parentTitle: 'Run output' },
    },
    contracts: new Map(),
  },
];

const demoContent: Record<string, ArtifactContent> = {
  'demo-release-v2': {
    source: '<main><style>body{margin:0;background:#0b1017;color:#e7eef7;font:16px/1.6 system-ui}main{max-width:760px;margin:0 auto;padding:56px}h1{font-size:36px;margin:0 0 8px}p{color:#9fb0c3}.tag{display:inline-block;padding:5px 9px;border-radius:999px;background:#18314c;color:#8bc5ff}section{margin-top:32px;padding:22px;border:1px solid #263748;border-radius:14px;background:#111923}li{margin:8px 0}</style><span class="tag">v2.0.0</span><h1>Agents Gateway release</h1><p>Capability-brokered agent runs with immutable artifacts and rootless isolation.</p><section><h2>What changed</h2><ul><li>Private per-run model and MCP broker</li><li>Read-only Skills Gateway materialization</li><li>Versioned artifact workspace</li></ul></section></main>',
  },
  'demo-release-v1': { source: '<main><h1>Agents Gateway release</h1><p>Initial release summary.</p></main>' },
  'demo-review-v1': { source: '# Review summary\n\nAll required checks passed.\n\n- Capability boundaries remain fail-closed.\n- Secrets stay on the trusted host.\n- Generated artifacts are immutable and tenant-scoped.\n' },
};

function presentationVersion(contract: ApiArtifactVersion, number: number): ArtifactVersion {
  return {
    id: contract.version_id,
    version: number,
    createdAt: contract.created_at,
    digest: contract.content.digest,
    sizeBytes: contract.content.size_bytes,
    mediaType: contract.content.media_type,
    contentKind: contract.manifest.content_kind,
    capabilities: contract.manifest.security.allow as ArtifactVersion['capabilities'],
  };
}

export function groupArtifactContracts(contracts: ApiArtifactVersion[]): ArtifactGroup[] {
  const grouped = new Map<string, ApiArtifactVersion[]>();
  for (const contract of contracts) {
    const versions = grouped.get(contract.artifact_id) ?? [];
    versions.push(contract);
    grouped.set(contract.artifact_id, versions);
  }
  return [...grouped.entries()].map(([artifactId, versions]) => {
    versions.sort((left, right) => Date.parse(right.created_at) - Date.parse(left.created_at));
    const latest = versions[0];
    const numberByID = new Map(versions.slice().reverse().map((version, index) => [version.version_id, index + 1]));
    const lineage = latest.lineage.forked_from
      ? { relation: 'fork' as const, parentArtifactId: latest.lineage.forked_from.artifact_id }
      : latest.lineage.parent_version_id
        ? { relation: 'derived' as const, parentArtifactId: artifactId, parentTitle: `Version ${latest.lineage.parent_version_id}` }
        : undefined;
    return {
      summary: {
        id: artifactId,
        title: latest.manifest.title,
        description: latest.manifest.description,
        contentKind: latest.manifest.content_kind,
        mediaType: latest.manifest.media_type,
        currentVersionId: latest.version_id,
        versions: versions.map((version) => presentationVersion(version, numberByID.get(version.version_id) ?? 1)),
        lineage,
      },
      contracts: new Map(versions.map((version) => [version.version_id, version])),
    };
  }).sort((left, right) => Date.parse(right.summary.versions[0]?.createdAt ?? '') - Date.parse(left.summary.versions[0]?.createdAt ?? ''));
}

function artifactIcon(summary: ArtifactSummary) {
  if (summary.contentKind === 'code') return Code2;
  if (summary.lineage) return GitBranch;
  return FileText;
}

function safeFilename(title: string) {
  const filename = title.replace(/[^a-zA-Z0-9._-]+/g, '-').replace(/^-+|-+$/g, '');
  return filename || 'artifact';
}

export function ArtifactsView({ api, liveMode, organization, project }: ArtifactsViewProps) {
  const [groups, setGroups] = useState<ArtifactGroup[]>(liveMode ? [] : demoArtifacts);
  const [listStatus, setListStatus] = useState<ArtifactContentStatus>(liveMode ? 'loading' : 'ready');
  const [listError, setListError] = useState('');
  const [selectedArtifactID, setSelectedArtifactID] = useState(liveMode ? '' : demoArtifacts[0].summary.id);
  const [selectedVersionID, setSelectedVersionID] = useState(liveMode ? '' : demoArtifacts[0].summary.currentVersionId ?? '');
  const [content, setContent] = useState<ArtifactContent | undefined>(liveMode ? undefined : demoContent[demoArtifacts[0].summary.currentVersionId ?? '']);
  const [contentStatus, setContentStatus] = useState<ArtifactContentStatus>(liveMode ? 'loading' : 'ready');
  const [contentError, setContentError] = useState('');

  useEffect(() => {
    if (!liveMode) {
      setGroups(demoArtifacts);
      setListStatus('ready');
      return;
    }
    let cancelled = false;
    setListStatus('loading');
    api.listArtifacts(organization, project).then((contracts) => {
      if (cancelled) return;
      const next = groupArtifactContracts(contracts);
      setGroups(next);
      setListStatus(next.length > 0 ? 'ready' : 'empty');
      if (next.length > 0) {
        setSelectedArtifactID(next[0].summary.id);
        setSelectedVersionID(next[0].summary.currentVersionId ?? next[0].summary.versions[0]?.id ?? '');
      }
    }).catch((error: unknown) => {
      if (!cancelled) {
        setListStatus('error');
        setListError(error instanceof Error ? error.message : 'Could not load artifacts.');
      }
    });
    return () => { cancelled = true; };
  }, [api, liveMode, organization, project]);

  const selectedGroup = useMemo(() => groups.find((group) => group.summary.id === selectedArtifactID) ?? groups[0], [groups, selectedArtifactID]);

  useEffect(() => {
    if (!selectedGroup || !selectedVersionID) {
      setContent(undefined);
      setContentStatus('empty');
      return;
    }
    if (!liveMode) {
      setContent(demoContent[selectedVersionID]);
      setContentStatus(demoContent[selectedVersionID] ? 'ready' : 'empty');
      return;
    }
    let cancelled = false;
    setContentStatus('loading');
    api.getArtifactContent(organization, project, selectedGroup.summary.id, selectedVersionID).then((result) => {
      if (!cancelled) {
        setContent({ source: result.body, preview: result.body });
        setContentStatus(result.body.length > 0 ? 'ready' : 'empty');
      }
    }).catch((error: unknown) => {
      if (!cancelled) {
        setContent(undefined);
        setContentStatus('error');
        setContentError(error instanceof Error ? error.message : 'Could not load artifact content.');
      }
    });
    return () => { cancelled = true; };
  }, [api, liveMode, organization, project, selectedGroup, selectedVersionID]);

  const selectArtifact = (group: ArtifactGroup) => {
    setSelectedArtifactID(group.summary.id);
    setSelectedVersionID(group.summary.currentVersionId ?? group.summary.versions[0]?.id ?? '');
  };

  const download = async (version: ArtifactVersion) => {
    const body = liveMode
      ? await api.downloadArtifact(organization, project, selectedGroup.summary.id, version.id)
      : new Blob([demoContent[version.id]?.source ?? ''], { type: version.mediaType ?? selectedGroup.summary.mediaType });
    const url = URL.createObjectURL(body);
    const link = document.createElement('a');
    link.href = url;
    link.download = safeFilename(selectedGroup.summary.title);
    link.click();
    URL.revokeObjectURL(url);
  };

  if (listStatus === 'loading') return <div className="state-panel" role="status"><Package size={18} /><strong>Loading artifacts</strong><span>Reading immutable versions from the catalog.</span></div>;
  if (listStatus === 'error') return <div className="state-panel state-error" role="alert"><Package size={18} /><strong>Artifacts unavailable</strong><span>{listError}</span></div>;
  if (!selectedGroup) return <div className="state-panel" role="status"><Package size={18} /><strong>No artifacts yet</strong><span>Completed agent runs will publish immutable outputs here.</span></div>;

  return <div className="artifact-browser">
    <aside className="section-card artifact-library" aria-label="Artifact library">
      <div className="section-heading"><div><div className="section-eyebrow">Immutable outputs</div><h2>Artifacts</h2></div><span className="artifact-count">{groups.length}</span></div>
      <div className="artifact-library-list">{groups.map((group) => {
        const Icon = artifactIcon(group.summary);
        const current = group.summary.versions[0];
        return <button key={group.summary.id} type="button" className={`artifact-library-item ${group.summary.id === selectedGroup.summary.id ? 'selected' : ''}`} onClick={() => selectArtifact(group)} aria-pressed={group.summary.id === selectedGroup.summary.id}>
          <span className="file-icon"><Icon size={15} /></span><span><strong>{group.summary.title}</strong><small>{current?.mediaType ?? group.summary.mediaType} · {group.summary.versions.length} version{group.summary.versions.length === 1 ? '' : 's'}</small></span>
        </button>;
      })}</div>
    </aside>
    <ArtifactWorkspace
      artifact={selectedGroup.summary}
      selectedVersionId={selectedVersionID}
      content={content}
      contentStatus={contentStatus}
      errorMessage={contentError}
      onVersionChange={(version) => setSelectedVersionID(version.id)}
      onDownload={(version) => { void download(version); }}
    />
  </div>;
}

export default ArtifactsView;
