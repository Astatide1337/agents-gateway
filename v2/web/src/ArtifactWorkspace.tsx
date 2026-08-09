import { useEffect, useMemo, useState } from 'react';
import type { KeyboardEvent, ReactNode } from 'react';
import { AlertTriangle, Code2, Download, FileText, GitBranch, Package } from 'lucide-react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';

export type ArtifactContentKind =
  | 'document'
  | 'code'
  | 'single_page_html'
  | 'svg'
  | 'diagram'
  | 'interactive_component'
  | 'plaintext'
  | 'markdown'
  | 'html'
  | 'interactive';

export interface ArtifactLineage {
  relation?: 'fork' | 'derived' | 'remix';
  parentArtifactId: string;
  parentTitle?: string;
}

export interface ArtifactVersion {
  id: string;
  version: number | string;
  label?: string;
  createdAt?: string;
  digest?: string;
  sizeBytes?: number;
  mediaType?: string;
  contentKind?: ArtifactContentKind;
  capabilities?: readonly ('sandboxed_scripts' | 'network' | 'external_calls' | 'clipboard_write')[];
}

export interface ArtifactSummary {
  id: string;
  title: string;
  description?: string;
  contentKind: ArtifactContentKind;
  mediaType?: string;
  currentVersionId?: string;
  versions: readonly ArtifactVersion[];
  lineage?: ArtifactLineage;
}

export interface ArtifactContent {
  source: string;
  /** Optional representation to show in the Preview tab. Source remains available separately. */
  preview?: string;
}

export type ArtifactContentInput = ArtifactContent | string;
export type ArtifactContentStatus = 'ready' | 'loading' | 'empty' | 'error';
export type ArtifactDownloadUrl = string | ((version: ArtifactVersion) => string | undefined);

export interface ArtifactWorkspaceProps {
  artifact: ArtifactSummary;
  content?: ArtifactContentInput;
  contentStatus?: ArtifactContentStatus;
  errorMessage?: string;
  selectedVersionId?: string;
  downloadUrl?: ArtifactDownloadUrl;
  onVersionChange?: (version: ArtifactVersion) => void;
  onDownload?: (version: ArtifactVersion) => void;
  className?: string;
}

const MAX_CONTENT_BYTES = 2_000_000;

const CONTENT_KIND_LABELS: Record<ArtifactContentKind, string> = {
  document: 'Document',
  code: 'Code',
  single_page_html: 'HTML',
  svg: 'SVG',
  diagram: 'Diagram',
  interactive_component: 'Interactive',
  plaintext: 'Plain text',
  markdown: 'Markdown',
  html: 'HTML',
  interactive: 'Interactive',
};

const PREVIEW_KINDS = new Set<ArtifactContentKind>([
  'single_page_html',
  'svg',
  'diagram',
  'interactive_component',
  'html',
  'interactive',
]);

const INTERACTIVE_KINDS = new Set<ArtifactContentKind>(['interactive_component', 'interactive']);

const RESTRICTIVE_CSP = [
  "default-src 'none'",
  "base-uri 'none'",
  "connect-src 'none'",
  'font-src data:',
  "form-action 'none'",
  "frame-ancestors 'none'",
  "frame-src 'none'",
  'img-src data: blob:',
  "manifest-src 'none'",
  "media-src 'none'",
  "object-src 'none'",
  "style-src 'unsafe-inline'",
  "worker-src 'none'",
].join('; ');

const INTERACTIVE_CSP = `${RESTRICTIVE_CSP}; script-src 'unsafe-inline'`;
const STATIC_CSP = `${RESTRICTIVE_CSP}; script-src 'none'`;

function formatBytes(bytes?: number) {
  if (bytes === undefined || !Number.isFinite(bytes) || bytes < 0) return undefined;
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(bytes < 10 * 1024 ? 1 : 0)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(bytes < 10 * 1024 * 1024 ? 1 : 0)} MB`;
}

function formatDate(value?: string) {
  if (!value) return undefined;
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat('en-US', { dateStyle: 'medium', timeStyle: 'short' }).format(date);
}

function versionLabel(version: ArtifactVersion) {
  return version.label ?? (typeof version.version === 'number' ? `Version ${version.version}` : version.version);
}

function kindLabel(kind: ArtifactContentKind) {
  return CONTENT_KIND_LABELS[kind] ?? kind;
}

function contentSizeBytes(value: string) {
  return new TextEncoder().encode(value).byteLength;
}

function getSource(content?: ArtifactContentInput) {
  return typeof content === 'string' ? content : content?.source;
}

function getPreview(content?: ArtifactContentInput) {
  return typeof content === 'string' ? content : content?.preview ?? content?.source;
}

/**
 * Builds the complete document used by the sandboxed preview iframe.
 * This is intentionally srcDoc-only; callers never receive an executable URL.
 */
export function buildArtifactPreviewDocument(content: string, kind: ArtifactContentKind, allowScripts = false) {
  const csp = INTERACTIVE_KINDS.has(kind) && allowScripts ? INTERACTIVE_CSP : STATIC_CSP;
  return `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="${csp}"><meta name="referrer" content="no-referrer"></head><body>${content}</body></html>`;
}

function PreviewFrame({ title, content, kind, allowScripts }: { title: string; content: string; kind: ArtifactContentKind; allowScripts: boolean }) {
  const srcDoc = useMemo(() => buildArtifactPreviewDocument(content, kind, allowScripts), [allowScripts, content, kind]);
  const interactive = INTERACTIVE_KINDS.has(kind) && allowScripts;

  return (
    <iframe
      className="artifact-preview-frame"
      title={`${title} preview`}
      srcDoc={srcDoc}
      sandbox={interactive ? 'allow-scripts' : ''}
      referrerPolicy="no-referrer"
    />
  );
}

function ContentState({ status, message }: { status: ArtifactContentStatus; message?: string }) {
  if (status === 'loading') {
    return <div className="state-panel" role="status"><Package size={18} /><strong>Loading artifact</strong><span>Fetching the selected immutable version.</span></div>;
  }
  if (status === 'error') {
    return <div className="state-panel state-error" role="alert"><AlertTriangle size={18} /><strong>Artifact unavailable</strong><span>{message || 'The artifact content could not be loaded.'}</span></div>;
  }
  return <div className="state-panel" role="status"><FileText size={18} /><strong>Nothing to display</strong><span>{message || 'This artifact version has no content.'}</span></div>;
}

function EmptyPreview({ kind }: { kind: ArtifactContentKind }) {
  return <div className="state-panel" role="status"><Code2 size={18} /><strong>Preview unavailable</strong><span>{kindLabel(kind)} content is available from the Source tab.</span></div>;
}

function TextContent({ value, label }: { value: string; label: string }) {
  return <pre className="artifact-source" aria-label={label}><code>{value}</code></pre>;
}

function MarkdownPreview({ value, label }: { value: string; label: string }) {
  return (
    <article className="artifact-markdown" aria-label={label}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        skipHtml
        components={{
          // Artifact previews are deny-by-default. Display link and image text
          // without navigable/fetching attributes so Markdown cannot trigger
          // network access from the host console.
          a: ({ children }) => <span className="artifact-markdown-link">{children}</span>,
          img: ({ alt }) => <span className="artifact-markdown-image">{alt ? `[Image: ${alt}]` : '[Image omitted]'}</span>,
        }}
      >
        {value}
      </ReactMarkdown>
    </article>
  );
}

function ArtifactMetadata({ artifact, version }: { artifact: ArtifactSummary; version: ArtifactVersion }) {
  const size = formatBytes(version.sizeBytes);
  const created = formatDate(version.createdAt);
  return (
    <dl className="detail-facts artifact-metadata">
      <div><dt>Type</dt><dd>{kindLabel(version.contentKind ?? artifact.contentKind)}</dd></div>
      {artifact.mediaType || version.mediaType ? <div><dt>Media type</dt><dd className="mono-text">{version.mediaType ?? artifact.mediaType}</dd></div> : null}
      {size ? <div><dt>Size</dt><dd>{size}</dd></div> : null}
      {created ? <div><dt>Created</dt><dd>{created}</dd></div> : null}
      {version.digest ? <div><dt>Digest</dt><dd className="mono-text">{version.digest}</dd></div> : null}
    </dl>
  );
}

function LineageLabel({ lineage }: { lineage?: ArtifactLineage }) {
  if (!lineage) return null;
  const relation = lineage.relation === 'remix' ? 'Remixed from' : lineage.relation === 'derived' ? 'Derived from' : 'Forked from';
  return <div className="artifact-lineage" role="note" aria-label="Artifact lineage"><GitBranch size={14} /><span>{relation} <strong>{lineage.parentTitle ?? lineage.parentArtifactId}</strong></span></div>;
}

function artifactStatus(content: ArtifactContentInput | undefined, requested: ArtifactContentStatus | undefined, errorMessage: string | undefined): { status: ArtifactContentStatus; message?: string } {
  if (requested) return { status: requested, message: errorMessage };
  if (content === undefined) return { status: 'empty' };
  const source = getSource(content);
  if (!source) return { status: 'empty' };
  if (contentSizeBytes(source) > MAX_CONTENT_BYTES) return { status: 'error', message: 'This artifact is too large to preview safely. Download it to inspect the full content.' };
  return { status: 'ready' };
}

export function ArtifactWorkspace({
  artifact,
  content,
  contentStatus,
  errorMessage,
  selectedVersionId,
  downloadUrl,
  onVersionChange,
  onDownload,
  className = '',
}: ArtifactWorkspaceProps) {
  const initialVersionId = artifact.currentVersionId ?? artifact.versions[0]?.id ?? '';
  const [internalVersionId, setInternalVersionId] = useState(initialVersionId);
  const [tab, setTab] = useState<'preview' | 'source'>('preview');

  useEffect(() => {
    if (selectedVersionId === undefined && artifact.versions.some((version) => version.id === initialVersionId)) {
      setInternalVersionId(initialVersionId);
    }
  }, [artifact.id, artifact.currentVersionId, artifact.versions, initialVersionId, selectedVersionId]);

  const activeVersionId = selectedVersionId ?? internalVersionId;
  const version = artifact.versions.find((item) => item.id === activeVersionId) ?? artifact.versions[0];
  const state = artifactStatus(content, contentStatus, errorMessage);
  const source = getSource(content);
  const preview = getPreview(content);
  const previewKind = version?.contentKind ?? artifact.contentKind;
  const previewMediaType = version?.mediaType ?? artifact.mediaType;
  const allowScripts = version?.capabilities?.includes('sandboxed_scripts') ?? false;
  const canPreview = PREVIEW_KINDS.has(previewKind);
  const isMarkdown = previewKind === 'markdown' || (previewKind === 'document' && previewMediaType === 'text/markdown');
  const resolvedDownloadUrl: string | undefined = typeof downloadUrl === 'function' ? downloadUrl(version) : downloadUrl;
  const headingId = `artifact-title-${artifact.id.replace(/[^a-zA-Z0-9_-]/g, '-')}`;
  const previewPanelId = `${headingId}-preview-panel`;
  const sourcePanelId = `${headingId}-source-panel`;
  const previewTabId = `${headingId}-preview-tab`;
  const sourceTabId = `${headingId}-source-tab`;

  if (!version) {
    return <section className={`section-card artifact-workspace ${className}`.trim()} aria-labelledby={headingId}><div className="section-heading"><div><h2 id={headingId}>{artifact.title}</h2></div></div><ContentState status="empty" message="No immutable versions are available for this artifact." /></section>;
  }

  const changeVersion = (nextId: string) => {
    const next = artifact.versions.find((item) => item.id === nextId);
    if (!next) return;
    if (selectedVersionId === undefined) setInternalVersionId(next.id);
    onVersionChange?.(next);
  };

  const moveTab = (direction: -1 | 1 | 'first' | 'last') => {
    const tabs = ['preview', 'source'] as const;
    const currentIndex = tabs.indexOf(tab);
    const nextIndex = direction === 'first' ? 0 : direction === 'last' ? tabs.length - 1 : (currentIndex + direction + tabs.length) % tabs.length;
    setTab(tabs[nextIndex]);
  };

  const handleTabKeyDown = (event: KeyboardEvent<HTMLButtonElement>) => {
    if (event.key === 'ArrowRight' || event.key === 'ArrowDown') {
      event.preventDefault();
      moveTab(1);
    } else if (event.key === 'ArrowLeft' || event.key === 'ArrowUp') {
      event.preventDefault();
      moveTab(-1);
    } else if (event.key === 'Home') {
      event.preventDefault();
      moveTab('first');
    } else if (event.key === 'End') {
      event.preventDefault();
      moveTab('last');
    }
  };

  const downloadButton: ReactNode = onDownload ? (
    <button className="secondary-button" type="button" onClick={() => onDownload(version)}><Download size={15} /> Download</button>
  ) : resolvedDownloadUrl ? (
    <a className="secondary-button" href={resolvedDownloadUrl} download={version.label ?? artifact.title} rel="noreferrer"> <Download size={15} /> Download</a>
  ) : (
    <button className="secondary-button" type="button" disabled aria-disabled="true"><Download size={15} /> Download unavailable</button>
  );

  return (
    <section className={`section-card artifact-workspace ${className}`.trim()} aria-labelledby={headingId}>
      <div className="section-heading">
        <div>
          <div className="section-eyebrow"><Package size={13} /> Artifact</div>
          <h2 id={headingId}>{artifact.title}</h2>
          {artifact.description ? <p className="muted-text">{artifact.description}</p> : null}
        </div>
        <div className="header-actions">
          {artifact.versions.length > 0 ? <label className="artifact-version-selector"><span className="sr-only">Artifact version</span><select aria-label="Artifact version" value={activeVersionId} onChange={(event) => changeVersion(event.target.value)} disabled={artifact.versions.length < 2}>{artifact.versions.map((item) => <option key={item.id} value={item.id}>{versionLabel(item)}</option>)}</select></label> : null}
          {downloadButton}
        </div>
      </div>

      <div className="filter-row artifact-summary-row">
        <ArtifactMetadata artifact={artifact} version={version} />
        <LineageLabel lineage={artifact.lineage} />
      </div>

      <div className="segmented artifact-tabs" role="tablist" aria-label="Artifact views">
        <button id={previewTabId} type="button" role="tab" aria-selected={tab === 'preview'} aria-controls={previewPanelId} tabIndex={tab === 'preview' ? 0 : -1} className={tab === 'preview' ? 'selected' : ''} onKeyDown={handleTabKeyDown} onClick={() => setTab('preview')}>Preview</button>
        <button id={sourceTabId} type="button" role="tab" aria-selected={tab === 'source'} aria-controls={sourcePanelId} tabIndex={tab === 'source' ? 0 : -1} className={tab === 'source' ? 'selected' : ''} onKeyDown={handleTabKeyDown} onClick={() => setTab('source')}>Source</button>
      </div>

      <div id={tab === 'preview' ? previewPanelId : sourcePanelId} role="tabpanel" aria-labelledby={tab === 'preview' ? previewTabId : sourceTabId} tabIndex={0}>
        {state.status !== 'ready' ? <ContentState status={state.status} message={state.message} /> : tab === 'source' ? <TextContent value={source ?? ''} label={`Source for ${artifact.title}`} /> : isMarkdown && preview ? <MarkdownPreview value={preview} label={`Markdown preview for ${artifact.title}`} /> : canPreview && preview ? <PreviewFrame title={artifact.title} content={preview} kind={previewKind} allowScripts={allowScripts} /> : canPreview ? <EmptyPreview kind={previewKind} /> : <TextContent value={source ?? ''} label={`Text preview for ${artifact.title}`} />}
      </div>
    </section>
  );
}

export default ArtifactWorkspace;
