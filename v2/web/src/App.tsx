import { useEffect, useMemo, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import type { LucideIcon } from 'lucide-react';
import {
  Activity, AlertTriangle, ArrowUpRight, Bot, Boxes, Check, CheckCircle2, ChevronDown, CircleAlert,
  CircleDot, Clock3, Cloud, Code2, Database, ExternalLink, FileCheck2, Gauge, GitBranch,
  KeyRound, Layers3, LayoutDashboard, ListChecks, LockKeyhole, Menu, MoreHorizontal,
  Network, Package, PlayCircle, Plus, RefreshCw, Search, Server, Settings2, ShieldCheck, Sparkles,
  TerminalSquare, UserRound, Workflow, XCircle, Zap,
} from 'lucide-react';
import { ApiError, createApiClient, type ApiAuditEntry, type ApiEvent, type ApiResource, type ApiRun, type ApiUsage, type RemoteState, type ApiClient } from './api';
import ArtifactsView from './ArtifactsView';

export const DEFAULT_ORGANIZATION = '00000000-0000-4000-8000-000000000001';
export const DEFAULT_PROJECT = '00000000-0000-4000-8000-000000000002';

type ViewId = 'overview' | 'definitions' | 'runs' | 'approvals' | 'artifacts' | 'entitlements' | 'quotas' | 'audit' | 'runners';

interface NavItem { id: ViewId; label: string; icon: LucideIcon; count?: number }
interface Definition { name: string; kind: 'Agent' | 'Workflow' | 'ToolSet' | 'SkillSet'; revision: string; digest: string; status: string; updated: string; summary: string }
interface Run { id: string; name: string; kind: 'WorkflowRun' | 'AgentRun'; status: string; duration: string; started: string; owner: string; definition: string; createdAt?: string }
interface Approval { id: string; title: string; context: string; requester: string; requested: string; risk: 'Low' | 'Medium' | 'High'; }
type LiveApproval = Approval & { runID: string; stepID: string };

interface LiveViewScope {
  api: ApiClient;
  liveMode: boolean;
  organization: string;
  project: string;
  refreshKey: number;
}

function apiErrorMessage(error: unknown): string {
  return error instanceof Error ? error.message : 'The control plane request failed.';
}

function apiStateFromErrors(errors: unknown[], emptyMessage: string): RemoteState<never> {
  if (errors.length === 0) return { status: 'empty', message: emptyMessage };
  if (errors.every((error) => error instanceof ApiError && (error.status === 401 || error.status === 403))) {
    return { status: 'permission', message: 'Your session is not authorized for one or more configured resources.' };
  }
  return { status: 'error', message: errors.map(apiErrorMessage).join(' · ') };
}

function useLiveCollection<T>(liveMode: boolean, refreshKey: number, loader: () => Promise<T[]>, emptyMessage: string) {
  const [state, setState] = useState<RemoteState<T[]>>({ status: 'loading' });
  const loaderRef = useRef(loader);
  loaderRef.current = loader;
  useEffect(() => {
    let cancelled = false;
    if (!liveMode) return () => { cancelled = true; };
    setState({ status: 'loading' });
    loaderRef.current().then((data) => {
      if (!cancelled) setState(data.length === 0 ? { status: 'empty', message: emptyMessage } : { status: 'ready', data });
    }).catch((error: unknown) => {
      if (!cancelled) setState(apiStateFromErrors([error], emptyMessage));
    });
    return () => { cancelled = true; };
  }, [emptyMessage, liveMode, refreshKey]);
  return state;
}

const navGroups: { label: string; items: NavItem[] }[] = [
  { label: 'Control plane', items: [
    { id: 'overview', label: 'Overview', icon: LayoutDashboard },
    { id: 'definitions', label: 'Definitions', icon: Boxes },
    { id: 'runs', label: 'Runs & workflows', icon: Workflow },
    { id: 'approvals', label: 'Approvals', icon: ShieldCheck },
  ] },
  { label: 'Resources', items: [
    { id: 'artifacts', label: 'Artifacts', icon: Package },
    { id: 'entitlements', label: 'Entitlements', icon: KeyRound },
    { id: 'quotas', label: 'Quotas & spend', icon: Gauge },
    { id: 'audit', label: 'Audit history', icon: FileCheck2 },
  ] },
  { label: 'Platform', items: [{ id: 'runners', label: 'Runners', icon: Server }] },
];

const definitions: Definition[] = [
  { name: 'issue-fixer', kind: 'Agent', revision: 'r24', digest: 'sha256:8f3a…d1c2', status: 'Ready', updated: '8 min ago', summary: 'Codex · coding-medium · 4 skills · 6 tools' },
  { name: 'fix-issue', kind: 'Workflow', revision: 'r12', digest: 'sha256:2b17…a90e', status: 'Ready', updated: '12 min ago', summary: 'Investigate → implement → review' },
  { name: 'github-issue-fixer', kind: 'ToolSet', revision: 'r7', digest: 'sha256:19dd…b44a', status: 'Ready', updated: '1 hour ago', summary: 'GitHub MCP · 6 allow-listed tools' },
  { name: 'engineering-baseline', kind: 'SkillSet', revision: 'r4', digest: 'sha256:0c91…73af', status: 'Draft', updated: '3 hours ago', summary: 'TDD · review · release checklist' },
  { name: 'dependency-upgrader', kind: 'Agent', revision: 'r9', digest: 'sha256:7f42…01ae', status: 'Attention', updated: 'Yesterday', summary: 'Claude Code · network policy needs review' },
];

const runs: Run[] = [
  { id: 'run-fix-issue-0842', name: 'Fix issue #842', kind: 'WorkflowRun', status: 'Running', duration: '08:42', started: '12 min ago', owner: 'soham', definition: 'fix-issue · r12' },
  { id: 'run-deps-0839', name: 'Upgrade dependencies', kind: 'AgentRun', status: 'WaitingApproval', duration: '23:08', started: '31 min ago', owner: 'sophie', definition: 'dependency-upgrader · r9' },
  { id: 'run-review-0837', name: 'Review PR #188', kind: 'AgentRun', status: 'Succeeded', duration: '04:16', started: '1 hour ago', owner: 'soham', definition: 'code-reviewer · r18' },
  { id: 'run-release-0833', name: 'Prepare release notes', kind: 'WorkflowRun', status: 'Succeeded', duration: '06:51', started: '2 hours ago', owner: 'ci-bot', definition: 'release-notes · r3' },
  { id: 'run-scan-0827', name: 'Security scan', kind: 'AgentRun', status: 'Failed', duration: '01:03', started: '3 hours ago', owner: 'soham', definition: 'security-scanner · r5' },
];

const approvals: Approval[] = [
  { id: 'apr-1048', title: 'Create pull request', context: 'issue-fixer wants to create a PR in agents-gateway', requester: 'run-fix-issue-0842', requested: '2 min ago', risk: 'Medium' },
  { id: 'apr-1047', title: 'Use organization API fallback', context: 'dependency-upgrader reached the owner subscription cooldown', requester: 'run-deps-0839', requested: '9 min ago', risk: 'Low' },
  { id: 'apr-1046', title: 'Push branch to origin', context: 'dependency-upgrader wants to push feat/deps-2026-08', requester: 'run-deps-0839', requested: '14 min ago', risk: 'High' },
];

const events: ApiEvent[] = [
  { sequence: 42, type: 'tool.completed', payload: { tool: 'github.get_issue', durationMs: 418, status: 'ok' }, createdAt: '2026-08-08T14:38:42Z' },
  { sequence: 41, type: 'tool.requested', payload: { tool: 'github.get_issue', resource: 'Astatide1337/agents-gateway#842' }, createdAt: '2026-08-08T14:38:41Z' },
  { sequence: 40, type: 'model.completed', payload: { model: 'gpt-5-codex', inputTokens: 2184, outputTokens: 782 }, createdAt: '2026-08-08T14:38:34Z' },
  { sequence: 39, type: 'verification.passed', payload: { command: 'go test ./...', durationMs: 12432 }, createdAt: '2026-08-08T14:37:59Z' },
  { sequence: 38, type: 'assistant.message', payload: { preview: 'Tests are green. I am preparing the change summary.' }, createdAt: '2026-08-08T14:37:44Z' },
  { sequence: 37, type: 'sandbox.started', payload: { backend: 'gvisor', isolation: 'enhanced/shared-kernel', image: 'agent-codex@sha256:8f3a…' }, createdAt: '2026-08-08T14:29:01Z' },
];

const auditEntries = [
  { action: 'approval.granted', subject: 'apr-1044 · run-review-0837', actor: 'soham', time: '42 min ago', result: 'Allowed' },
  { action: 'run.created', subject: 'run-fix-issue-0842', actor: 'soham', time: '12 min ago', result: 'Allowed' },
  { action: 'tool.denied', subject: 'github.delete_branch', actor: 'issue-fixer', time: '14 min ago', result: 'Denied' },
  { action: 'definition.applied', subject: 'Agent/issue-fixer · r24', actor: 'ci-bot', time: '8 min ago', result: 'Allowed' },
  { action: 'runner.enrolled', subject: 'runner/sohim-home', actor: 'instance-admin', time: 'Yesterday', result: 'Allowed' },
];

const navTitle: Record<ViewId, { eyebrow: string; title: string; description: string }> = {
  overview: { eyebrow: 'Control plane', title: 'Overview', description: 'A clear read on the health, safety, and throughput of your agent platform.' },
  definitions: { eyebrow: 'Desired state', title: 'Definitions', description: 'Immutable agent, workflow, capability, and skill revisions from Git.' },
  runs: { eyebrow: 'Execution', title: 'Runs & workflows', description: 'Follow live work from the DAG down to individual model and tool events.' },
  approvals: { eyebrow: 'Policy gate', title: 'Approvals', description: 'Review side effects before they cross the capability boundary.' },
  artifacts: { eyebrow: 'Output', title: 'Artifacts', description: 'Verified patches, reports, and run outputs with retention context.' },
  entitlements: { eyebrow: 'Model access', title: 'Entitlements & routes', description: 'See who can run which models and how fallback decisions are made.' },
  quotas: { eyebrow: 'Governance', title: 'Quotas & spend', description: 'Keep organizations predictable with explicit resource and spend limits.' },
  audit: { eyebrow: 'Trust layer', title: 'Audit history', description: 'An append-only trail of decisions, effects, and administrative actions.' },
  runners: { eyebrow: 'Execution plane', title: 'Runners', description: 'Check capacity, isolation posture, and the health of every execution host.' },
};

function formatTime(value: string) {
  return new Intl.DateTimeFormat('en-US', { hour: 'numeric', minute: '2-digit' }).format(new Date(value));
}

function StatusPill({ status }: { status: string }) {
  const normalized = status.toLowerCase().replace(/[_ ]/g, '-');
  const icon = normalized.includes('succeed') || normalized === 'ready' || normalized === 'allowed' ? <CheckCircle2 size={13} /> : normalized.includes('fail') || normalized.includes('attention') || normalized === 'denied' ? <XCircle size={13} /> : normalized.includes('wait') || normalized === 'draft' ? <Clock3 size={13} /> : <CircleDot size={13} />;
  return <span className={`status-pill status-${normalized}`}><span aria-hidden="true">{icon}</span>{status}</span>;
}

function MetricCard({ label, value, detail, tone = 'neutral', icon: Icon }: { label: string; value: string; detail: string; tone?: string; icon: LucideIcon }) {
  return <article className="metric-card">
    <div className="metric-top"><span className="metric-label">{label}</span><span className={`metric-icon metric-${tone}`}><Icon size={16} /></span></div>
    <div className="metric-value">{value}</div>
    <div className="metric-detail"><span className={`trend trend-${tone}`}>{detail.split(' · ')[0]}</span>{detail.includes(' · ') ? ` · ${detail.split(' · ').slice(1).join(' · ')}` : null}</div>
  </article>;
}

function SectionCard({ title, eyebrow, action, children, className = '' }: { title: string; eyebrow?: string; action?: ReactNode; children: ReactNode; className?: string }) {
  return <section className={`section-card ${className}`}>
    <div className="section-heading"> <div>{eyebrow ? <div className="section-eyebrow">{eyebrow}</div> : null}<h2>{title}</h2></div>{action}</div>
    {children}
  </section>;
}

function StatePanel({ state }: { state: RemoteState<unknown> }) {
  if (state.status === 'loading') return <div className="state-panel"><RefreshCw className="spin" size={20} /><strong>Connecting to the control plane…</strong><span>Checking readiness and permissions.</span></div>;
  if (state.status === 'permission') return <div className="state-panel state-permission"><LockKeyhole size={20} /><strong>Permission required</strong><span>{state.message}</span></div>;
  if (state.status === 'error') return <div className="state-panel state-error"><CircleAlert size={20} /><strong>Control plane unavailable</strong><span>{state.message}</span></div>;
  if (state.status === 'unavailable') return <div className="state-panel state-error"><CircleAlert size={20} /><strong>Live endpoint unavailable</strong><span>{state.message}</span></div>;
  if (state.status === 'empty') return <div className="state-panel"><Package size={20} /><strong>Nothing here yet</strong><span>{state.message ?? 'Apply a definition from Git or the CLI to populate this view.'}</span></div>;
  return null;
}

function Sidebar({ active, onNavigate, liveMode, organization, project, onDisconnect }: { active: ViewId; onNavigate: (view: ViewId) => void; liveMode: boolean; organization: string; project: string; onDisconnect?: () => void }) {
  return <aside className="sidebar">
    <div className="brand"><div className="brand-mark"><Sparkles size={17} /></div><div><strong>Agents Gateway</strong><span>Control plane</span></div></div>
    <button className="scope-switcher" aria-label="Change organization and project"><span className="scope-avatar">{organization.slice(0, 1).toUpperCase()}</span><span className="scope-copy"><strong>{organization}</strong><small>{project}</small></span><ChevronDown size={15} /></button>
    <nav aria-label="Primary navigation" className="nav-groups">
      {navGroups.map((group) => <div className="nav-group" key={group.label}><div className="nav-group-label">{group.label}</div>{group.items.map(({ id, label, icon: Icon, count }) => <button key={id} className={`nav-item ${active === id ? 'active' : ''}`} onClick={() => onNavigate(id)} aria-current={active === id ? 'page' : undefined}><Icon size={17} /><span>{label}</span>{count ? <span className="nav-count">{count}</span> : null}</button>)}</div>)}
    </nav>
    <div className="sidebar-bottom"><div className="runner-mini"><span className={liveMode ? 'online-dot' : 'offline-dot'} /><div><strong>{liveMode ? 'Live runner status' : 'Demo: 3 runners online'}</strong><small>{liveMode ? 'Open Runners for resource details' : 'Illustrative status only'}</small></div><ArrowUpRight size={14} /></div>{onDisconnect ? <button className="nav-item" onClick={onDisconnect}><LockKeyhole size={17} /><span>Disconnect token</span></button> : <button className="nav-item"><Settings2 size={17} /><span>Settings</span></button>}<div className="profile"><span className="avatar">S</span><div><strong>Soham</strong><small>Instance admin</small></div><MoreHorizontal size={16} /></div></div>
  </aside>;
}

function TopBar({ onMenu, onRefresh, search, setSearch, liveMode, organization, project }: { onMenu: () => void; onRefresh: () => void; search: string; setSearch: (value: string) => void; liveMode: boolean; organization: string; project: string }) {
  return <header className="topbar"><button className="mobile-menu" aria-label="Open navigation" onClick={onMenu}><Menu size={20} /></button><div className="breadcrumbs"><span>Organization</span><ChevronDown size={13} /><strong>{organization}</strong><span>/</span><strong>{project}</strong></div><div className="topbar-actions"><label className="search-field"><Search size={16} /><span className="sr-only">Search console</span><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder="Search runs, definitions…" /><kbd>⌘ K</kbd></label><button className="icon-button" aria-label="Refresh console" onClick={onRefresh}><RefreshCw size={17} /></button><div className="top-status"><span className={liveMode ? 'online-dot' : 'offline-dot'} /> {liveMode ? 'API mode' : 'Demo mode'}</div><span className="avatar avatar-small">S</span></div></header>;
}

function PageHeader({ view, onRun }: { view: ViewId; onRun: () => void }) {
  const copy = navTitle[view];
  return <div className="page-header"><div><div className="eyebrow"><span className="eyebrow-dot" />{copy.eyebrow}</div><h1>{copy.title}</h1><p>{copy.description}</p></div>{view === 'overview' || view === 'runs' ? <button className="primary-button" onClick={onRun}><Plus size={16} /> Run an agent</button> : null}</div>;
}

interface LiveRunnerSummary {
  name: string;
  host: string;
  backend: string;
  grade: string;
  load: string;
  ready: boolean;
  heartbeat: string;
}

interface LiveOverviewData {
  runs: RemoteState<Run[]>;
  runners: RemoteState<LiveRunnerSummary[]>;
  approvals: RemoteState<LiveApproval[]>;
  usage: RemoteState<ApiUsage>;
  audit: RemoteState<ApiAuditEntry[]>;
}

function overviewPanel<T>(result: PromiseSettledResult<T>, emptyMessage: string, empty: (value: T) => boolean): RemoteState<T> {
  if (result.status === 'rejected') {
    const error = result.reason;
    if (error instanceof ApiError && (error.status === 401 || error.status === 403)) return { status: 'permission', message: 'Your session cannot read this live overview resource.' };
    return { status: 'error', message: apiErrorMessage(error) };
  }
  return empty(result.value) ? { status: 'empty', message: emptyMessage } : { status: 'ready', data: result.value };
}

function useLiveOverview({ api, liveMode, organization, project, refreshKey }: LiveViewScope): RemoteState<LiveOverviewData> {
  const [state, setState] = useState<RemoteState<LiveOverviewData>>({ status: 'loading' });
  useEffect(() => {
    let cancelled = false;
    if (!liveMode) return () => { cancelled = true; };
    setState({ status: 'loading' });
    const runsPromise = api.listRuns(organization, project).then((items) => items.map(runFromApi));
    const runnersPromise = api.listRunners(organization, project).then((items) => items.map(liveRunnerFromResource));
    const approvalsPromise = api.listApprovals(organization, project).then((items) => items.map(approvalFromResource).filter((item): item is LiveApproval => item !== null && Boolean(item.runID)));
    const usagePromise = api.getUsage(organization, project).then((usage) => {
      if (!usage || !Number.isFinite(usage.activeRuns) || !Number.isFinite(usage.totalRuns) || !Number.isFinite(usage.artifactVersions) || !Number.isFinite(usage.artifactBytes)) throw new Error('The usage endpoint returned an invalid live response.');
      return usage;
    });
    const auditPromise = api.listAudit(organization, project);
    Promise.allSettled([runsPromise, runnersPromise, approvalsPromise, usagePromise, auditPromise]).then(([runsResult, runnersResult, approvalsResult, usageResult, auditResult]) => {
      if (cancelled) return;
      const data: LiveOverviewData = {
        runs: overviewPanel(runsResult, 'No runs have been recorded for this project yet.', (items) => items.length === 0),
        runners: overviewPanel(runnersResult, 'No runners have been enrolled for this project yet.', (items) => items.length === 0),
        approvals: overviewPanel(approvalsResult, 'No pending approvals were returned for this project.', (items) => items.length === 0),
        usage: overviewPanel(usageResult, 'The control plane has not reported usage for this project yet.', () => false),
        audit: overviewPanel(auditResult, 'No audit entries have been recorded for this project yet.', (items) => items.length === 0),
      };
      if (Object.values(data).every((panel) => panel.status !== 'ready' && panel.status !== 'empty')) {
        const messages = Object.values(data).map((panel) => panel.status === 'permission' || panel.status === 'error' ? panel.message : '').filter(Boolean);
        setState({ status: 'error', message: messages.join(' · ') || 'Live overview resources are unavailable.' });
      } else {
        setState({ status: 'ready', data });
      }
    });
    return () => { cancelled = true; };
  }, [api, liveMode, organization, project, refreshKey]);
  return state;
}

function LiveOverview({ api, liveMode, organization, project, refreshKey, onView, readiness }: LiveViewScope & { onView: (view: ViewId) => void; readiness: RemoteState<{ status: string }> }) {
  const state = useLiveOverview({ api, liveMode, organization, project, refreshKey });
  if (state.status !== 'ready') return <StatePanel state={state} />;
  const { runs, runners, approvals, usage, audit } = state.data;
  const panelErrors = [runs, runners, approvals, usage, audit].filter((panel) => panel.status === 'error' || panel.status === 'permission');
  const liveRuns = runs.status === 'ready' ? runs.data : [];
  const liveRunners = runners.status === 'ready' ? runners.data : [];
  const pendingApprovals = approvals.status === 'ready' ? approvals.data : [];
  const liveAudit = audit.status === 'ready' ? audit.data : [];
  const activeRunCount = usage.status === 'ready' ? usage.data.activeRuns : undefined;
  const totalRunCount = usage.status === 'ready' ? usage.data.totalRuns : undefined;
  const readyRunnerCount = liveRunners.filter((runner) => runner.ready).length;
  const failedRunCount = liveRuns.filter((run) => ['failed', 'lost'].includes(run.status.toLowerCase())).length;
  const unhealthyRunnerCount = liveRunners.filter((runner) => !runner.ready).length;
  const deniedAuditCount = liveAudit.filter((entry) => entry.decision.toLowerCase() === 'denied').length;
  const readinessReady = readiness.status === 'ready';
  const healthKnown = readinessReady && runners.status === 'ready';
  const healthPercent = healthKnown && liveRunners.length > 0 ? Math.round((readyRunnerCount / liveRunners.length) * 100) : 0;
  const healthValue = healthKnown ? (liveRunners.length > 0 ? String(healthPercent) : '—') : '—';
  const attentionCount = pendingApprovals.length + failedRunCount + unhealthyRunnerCount + deniedAuditCount;
  return <div className="view-stack">
    {panelErrors.length > 0 ? <div className="state-panel state-error"><CircleAlert size={18} /><strong>Partial live overview</strong><span>Some operational resources could not be read. Unavailable panels are marked below; no fallback snapshot is being shown.</span></div> : null}
    <div className="metric-grid"><MetricCard label="Active runs" value={activeRunCount === undefined ? '—' : String(activeRunCount)} detail={totalRunCount === undefined ? 'Live usage unavailable' : `${totalRunCount} total recorded`} tone="blue" icon={Activity} /><MetricCard label="Recorded runs" value={totalRunCount === undefined ? '—' : String(totalRunCount)} detail={liveRuns.length > 0 ? `${liveRuns.length} returned in recent collection` : 'Recent collection empty'} tone="green" icon={CheckCircle2} /><MetricCard label="Artifact storage" value={usage.status === 'ready' ? formatBytes(usage.data.artifactBytes) : '—'} detail={usage.status === 'ready' ? `${usage.data.artifactVersions} versions reported` : 'Live usage unavailable'} tone="purple" icon={Package} /><MetricCard label="Audit entries" value={audit.status === 'ready' ? String(liveAudit.length) : '—'} detail={deniedAuditCount > 0 ? `${deniedAuditCount} denied decision${deniedAuditCount === 1 ? '' : 's'}` : 'Live audit collection'} tone="amber" icon={FileCheck2} /></div>
    <div className="overview-grid"><SectionCard title="System readiness" eyebrow="Live control-plane signals" action={<button className="text-button" onClick={() => onView('runners')}>View runners <ArrowUpRight size={14} /></button>}><div className="health-summary"><div className="health-score"><div className="health-ring" style={{ background: `radial-gradient(circle at center, var(--surface) 57%, transparent 58%), conic-gradient(var(--green) 0 ${healthPercent}%, #20302d ${healthPercent}% 100%)` }}><span>{healthValue}</span><small>{healthKnown && liveRunners.length > 0 ? '/100' : 'status'}</small></div><div><strong>{readinessReady ? (runners.status === 'ready' && unhealthyRunnerCount === 0 ? 'Ready' : 'Attention') : 'Unknown'}</strong><p>{readinessReady ? 'Readiness endpoint is responding.' : 'Readiness has not returned a usable status.'}</p></div></div><div className="health-list"><HealthRow label="Control plane API" value={readiness.status === 'ready' ? readiness.data.status : 'Unavailable'} detail="Live readiness endpoint" tone={readinessReady ? 'healthy' : 'error'} /><HealthRow label="Runner fleet" value={runners.status === 'ready' ? `${readyRunnerCount} / ${liveRunners.length} ready` : 'Unavailable'} detail="Live Runner resources" tone={runners.status === 'ready' && unhealthyRunnerCount === 0 ? 'healthy' : 'warning'} /><HealthRow label="Approval queue" value={approvals.status === 'ready' ? `${pendingApprovals.length} pending` : 'Unavailable'} detail="Live Approval resources" tone={approvals.status === 'ready' && pendingApprovals.length === 0 ? 'healthy' : 'warning'} /><HealthRow label="Audit stream" value={audit.status === 'ready' ? `${liveAudit.length} recorded` : 'Unavailable'} detail="Live audit collection" tone={audit.status === 'ready' ? 'healthy' : 'error'} /></div></div></SectionCard><SectionCard title="Live activity" eyebrow="Recent project run collection"><div className="chart-header"><strong>{liveRuns.length}</strong><span>runs returned</span><span className="chart-change">{liveRuns.filter((run) => ['succeeded', 'completed'].includes(run.status.toLowerCase())).length} succeeded</span></div><div className="health-list activity-summary"><HealthRow label="Running" value={String(liveRuns.filter((run) => run.status.toLowerCase() === 'running').length)} detail="Current collection" tone="healthy" /><HealthRow label="Waiting approval" value={String(liveRuns.filter((run) => run.status.toLowerCase() === 'waitingapproval').length)} detail="Current collection" tone="warning" /><HealthRow label="Failed or lost" value={String(failedRunCount)} detail="Current collection" tone={failedRunCount > 0 ? 'error' : 'healthy'} /></div>{runs.status !== 'ready' ? <StatePanel state={runs} /> : null}</SectionCard></div>
    <div className="overview-grid lower-grid"><SectionCard title="Recent runs" eyebrow="Live project collection · newest first" action={<button className="text-button" onClick={() => onView('runs')}>View all <ArrowUpRight size={14} /></button>}>{runs.status === 'ready' ? (liveRuns.length > 0 ? <RunTable compact data={liveRuns} onSelect={() => onView('runs')} /> : <StatePanel state={runs} />) : <StatePanel state={runs} />}</SectionCard><SectionCard title="Attention required" eyebrow="Live approvals, runs, runners, and audit"><div className="attention-list">{approvals.status !== 'ready' ? <StatePanel state={approvals} /> : pendingApprovals.length > 0 ? <AttentionRow icon={ShieldCheck} tone="amber" title={`${pendingApprovals.length} approval${pendingApprovals.length === 1 ? '' : 's'} waiting`} detail="Side effects are paused" action="Review" onClick={() => onView('approvals')} /> : null}{runs.status === 'ready' && failedRunCount > 0 ? <AttentionRow icon={AlertTriangle} tone="red" title={`${failedRunCount} run${failedRunCount === 1 ? '' : 's'} needs attention`} detail="Failed or lost in the live run collection" action="Inspect" onClick={() => onView('runs')} /> : null}{runners.status === 'ready' && unhealthyRunnerCount > 0 ? <AttentionRow icon={Server} tone="red" title={`${unhealthyRunnerCount} runner${unhealthyRunnerCount === 1 ? '' : 's'} not ready`} detail="Runner resource readiness is not healthy" action="View" onClick={() => onView('runners')} /> : null}{audit.status === 'ready' && deniedAuditCount > 0 ? <AttentionRow icon={FileCheck2} tone="red" title={`${deniedAuditCount} denied audit decision${deniedAuditCount === 1 ? '' : 's'}`} detail="Review the live audit history" action="Review" onClick={() => onView('audit')} /> : null}{attentionCount === 0 && panelErrors.length === 0 ? <StatePanel state={{ status: 'empty', message: 'No pending approvals, failed runs, unhealthy runners, or denied audit decisions were found.' }} /> : null}</div></SectionCard></div>
  </div>;
}

function DemoOverview({ onView }: { onView: (view: ViewId) => void }) {
  return <div className="view-stack">
    <div className="metric-grid"><MetricCard label="Active runs" value="4" detail="↑ 18% · vs last 7 days" tone="blue" icon={Activity} /><MetricCard label="Success rate" value="96.4%" detail="↑ 2.8% · last 30 runs" tone="green" icon={CheckCircle2} /><MetricCard label="Tool calls" value="1,284" detail="↑ 14% · this week" tone="purple" icon={Zap} /><MetricCard label="Model spend" value="$18.42" detail="↓ 11% · this month" tone="amber" icon={Cloud} /></div>
    <div className="overview-grid"><SectionCard title="System health" eyebrow="Demo snapshot · not live health" action={<button className="text-button" onClick={() => onView('runners')}>View runners <ArrowUpRight size={14} /></button>}><div className="health-summary"><div className="health-score"><div className="health-ring"><span>98</span><small>/100</small></div><div><strong>Demo healthy</strong><p>Illustrative status only; this is not a live health signal.</p></div></div><div className="health-list"><HealthRow label="Control plane API" value="Demo operational" detail="42ms p95" /><HealthRow label="Workflow engine" value="Demo operational" detail="3 workers" /><HealthRow label="Capability broker" value="Demo operational" detail="0 denied spikes" /><HealthRow label="Runner fleet" value="Demo operational" detail="3 / 3 online" /></div></div></SectionCard><SectionCard title="Workflow throughput" eyebrow="Demo data · last 7 days"><div className="chart-header"><strong>47</strong><span>completed runs</span><span className="chart-change">+21.5%</span></div><div className="bar-chart" aria-label="Demo workflow throughput chart">{[32, 46, 38, 59, 54, 72, 86, 67, 78, 91, 73, 98, 88, 100].map((height, index) => <span key={index} style={{ height: `${height}%` }} className={index > 10 ? 'bar-current' : ''} />)}</div><div className="chart-axis"><span>Aug 2</span><span>Aug 5</span><span>Aug 8</span></div></SectionCard></div>
    <div className="overview-grid lower-grid"><SectionCard title="Recent runs" eyebrow="Demo data · across all workflows" action={<button className="text-button" onClick={() => onView('runs')}>View all <ArrowUpRight size={14} /></button>}><RunTable compact onSelect={() => onView('runs')} /></SectionCard><SectionCard title="Attention required" eyebrow="Demo data · policy gates and incidents"><div className="attention-list"><AttentionRow icon={ShieldCheck} tone="amber" title="3 approvals waiting" detail="Side effects are paused" action="Review" onClick={() => onView('approvals')} /><AttentionRow icon={AlertTriangle} tone="red" title="1 run needs attention" detail="security-scan · failed verification" action="Inspect" onClick={() => onView('runs')} /><AttentionRow icon={Server} tone="green" title="Demo runner fleet healthy" detail="3 online · illustrative isolation status" action="View" onClick={() => onView('runners')} /></div></SectionCard></div>
  </div>;
}

function Overview(props: LiveViewScope & { onView: (view: ViewId) => void; readiness: RemoteState<{ status: string }> }) {
  return props.liveMode ? <LiveOverview {...props} /> : <DemoOverview onView={props.onView} />;
}

function HealthRow({ label, value, detail, tone = 'healthy' }: { label: string; value: string; detail: string; tone?: 'healthy' | 'warning' | 'error' }) { const color = tone === 'error' ? 'var(--red)' : tone === 'warning' ? 'var(--amber)' : 'var(--green)'; return <div className="health-row"><span className={`health-indicator ${tone}`} style={{ background: color }} /><span className="health-label">{label}</span><strong>{value}</strong><small>{detail}</small></div>; }
function AttentionRow({ icon: Icon, tone, title, detail, action, onClick }: { icon: LucideIcon; tone: string; title: string; detail: string; action: string; onClick: () => void }) { return <div className="attention-row"><span className={`attention-icon ${tone}`}><Icon size={17} /></span><div><strong>{title}</strong><small>{detail}</small></div><button className="ghost-button" onClick={onClick}>{action}<ArrowUpRight size={13} /></button></div>; }

function RunTable({ compact = false, onSelect, data = runs }: { compact?: boolean; onSelect: (run: Run) => void; data?: Run[] }) {
  const rows = compact ? data.slice(0, 4) : data;
  return <div className="table-wrap"><table><caption className="sr-only">Agent runs</caption><thead><tr><th>Run</th><th>Status</th>{!compact ? <th>Definition</th> : null}<th>Duration</th><th>Started</th><th><span className="sr-only">Actions</span></th></tr></thead><tbody>{rows.map((run) => <tr key={run.id} onClick={() => onSelect(run)} className="clickable-row"><td><div className="run-cell"><span className={`run-type ${run.kind === 'WorkflowRun' ? 'workflow' : 'agent'}`}>{run.kind === 'WorkflowRun' ? <Workflow size={14} /> : <Bot size={14} />}</span><div><strong>{run.name}</strong><small>{run.id}</small></div></div></td><td><StatusPill status={run.status} /></td>{!compact ? <td><span className="muted-text">{run.definition}</span></td> : null}<td><span className="mono-text">{run.duration}</span></td><td><span className="muted-text">{run.started}</span></td><td><button className="icon-button table-action" aria-label={`Open ${run.name}`}><ArrowUpRight size={15} /></button></td></tr>)}</tbody></table></div>;
}

function definitionFromResource(resource: ApiResource<Record<string, unknown>>): Definition {
  const document = resource.document;
  const metadata = (document.metadata ?? {}) as Record<string, unknown>;
  const spec = (document.spec ?? {}) as Record<string, unknown>;
  const kind = (resource.kind === 'Agent' || resource.kind === 'Workflow' || resource.kind === 'ToolSet' || resource.kind === 'SkillSet' ? resource.kind : 'Agent') as Definition['kind'];
  const details = kind === 'Agent'
    ? `${String(spec.runtime && typeof spec.runtime === 'object' ? ((spec.runtime as Record<string, unknown>).harness ?? 'agent') : 'agent')} · ${Array.isArray(spec.skills) ? spec.skills.length : 0} skills · ${spec.toolSetRef ? '1 tool set' : 'no tool set'}`
    : kind === 'Workflow'
      ? `${Array.isArray(spec.steps) ? spec.steps.length : 0} workflow steps`
      : kind === 'ToolSet'
        ? `${Array.isArray(spec.servers) ? spec.servers.length : 0} MCP servers`
        : `${Array.isArray(spec.skills) ? spec.skills.length : 0} skills`;
  return {
    name: String(metadata.name ?? resource.name),
    kind,
    revision: `r${resource.revision}`,
    digest: resource.digest,
    status: 'Ready',
    updated: resource.createdAt ? formatTime(resource.createdAt) : 'Unknown',
    summary: details,
  };
}

function LiveDefinitionsView({ api, liveMode, organization, project, refreshKey }: LiveViewScope) {
  const state = useLiveCollection(liveMode, refreshKey, () => api.listDefinitions(organization, project), 'No definitions have been applied to this project yet.');
  if (state.status !== 'ready') return <StatePanel state={state} />;
  const liveDefinitions = state.data.map(definitionFromResource);
  if (liveDefinitions.length === 0) return <StatePanel state={{ status: 'empty', message: 'No configured definitions were returned by the control plane.' }} />;
  return <DefinitionsContent definitions={liveDefinitions} live />;
}

function DefinitionsContent({ definitions: rows, live = false }: { definitions: Definition[]; live?: boolean }) {
  const [selected, setSelected] = useState(rows[0]);
  useEffect(() => { setSelected(rows[0]); }, [rows]);
  return <div className="two-column-view"><SectionCard title="Definitions" eyebrow={live ? 'Live applied resources' : 'Source of truth: Git'} action={live ? null : <button className="secondary-button"><ExternalLink size={15} /> Open repository</button>} className="definitions-table"><div className="filter-row"><div className="segmented"><button className="selected">All <span>{rows.length}</span></button><button>Agents <span>{rows.filter((definition) => definition.kind === 'Agent').length}</span></button><button>Workflows <span>{rows.filter((definition) => definition.kind === 'Workflow').length}</span></button><button>Capabilities <span>{rows.filter((definition) => definition.kind === 'ToolSet' || definition.kind === 'SkillSet').length}</span></button></div><button className="icon-button" aria-label="Filter definitions"><ListChecks size={17} /></button></div><div className="definition-list">{rows.map((definition) => <button className={`definition-row ${selected.name === definition.name ? 'selected' : ''}`} key={`${definition.kind}-${definition.name}`} onClick={() => setSelected(definition)}><span className={`definition-icon kind-${definition.kind.toLowerCase()}`}>{definition.kind === 'Agent' ? <Bot size={17} /> : definition.kind === 'Workflow' ? <Workflow size={17} /> : definition.kind === 'ToolSet' ? <Zap size={17} /> : <Sparkles size={17} />}</span><span className="definition-main"><strong>{definition.name}</strong><small>{definition.summary}</small></span><span className="definition-revision"><strong>{definition.revision}</strong><small>{definition.updated}</small></span><StatusPill status={definition.status} /></button>)}</div></SectionCard><DefinitionDetail definition={selected} live={live} /></div>;
}

function DefinitionsView({ api, liveMode, organization, project, refreshKey }: LiveViewScope) {
  if (liveMode) return <LiveDefinitionsView api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} />;
  return <DefinitionsContent definitions={definitions} />;
}

function DefinitionDetail({ definition, live = false }: { definition: Definition; live?: boolean }) { return <SectionCard title="Revision detail" eyebrow="Immutable applied revision" className="detail-card"><div className="detail-title"><span className={`definition-icon kind-${definition.kind.toLowerCase()}`}>{definition.kind === 'Agent' ? <Bot size={20} /> : <Workflow size={20} />}</span><div><h3>{definition.name}</h3><p>{definition.kind} · {live ? 'read from control plane' : 'applied from Git'}</p></div><StatusPill status={definition.status} /></div><div className="detail-facts"><Fact label="Revision" value={definition.revision} /><Fact label="Digest" value={definition.digest} mono /><Fact label="Last applied" value={definition.updated} /></div><div className="code-snippet"><div className="code-head"><span>agents.astatide.com/v1alpha1</span><button className="icon-button" aria-label="Copy revision digest"><Code2 size={14} /></button></div><pre><code>{`kind: ${definition.kind}\nmetadata:\n  name: ${definition.name}\nspec:\n  source: git\n  revision: ${definition.revision}\n  digest: ${definition.digest}`}</code></pre></div>{live ? null : <div className="detail-actions"><button className="secondary-button"><GitBranch size={15} /> View in Git</button><button className="ghost-button">Revision history <ArrowUpRight size={13} /></button></div>}</SectionCard>; }
function Fact({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) { return <div><span>{label}</span><strong className={mono ? 'mono-text' : ''}>{value}</strong></div>; }

function runFromApi(run: ApiRun): Run {
  return {
    id: run.id,
    name: run.id,
    kind: run.kind === 'WorkflowRun' ? 'WorkflowRun' : 'AgentRun',
    status: run.status,
    duration: '—',
    started: run.createdAt ? formatTime(run.createdAt) : 'Unknown',
    owner: run.requestedBy ?? 'Unknown',
    definition: run.definitionDigest,
    createdAt: run.createdAt,
  };
}

function useLiveRuns(api: ApiClient, liveMode: boolean, organization: string, project: string, refreshKey: number) {
  return useLiveCollection(liveMode, refreshKey, () => api.listRuns(organization, project).then((items) => items.map(runFromApi)), 'No runs have been recorded for this project yet.');
}

function LiveEventsPanel({ api, organization, project, run, refreshKey }: LiveViewScope & { run: Run }) {
  const [state, setState] = useState<RemoteState<ApiEvent[]>>({ status: 'loading' });
  useEffect(() => {
    let cancelled = false;
    setState({ status: 'loading' });
    api.listEvents(organization, project, run.id).then((data) => {
      if (!cancelled) setState(data.length === 0 ? { status: 'empty', message: 'This run has not emitted any events yet.' } : { status: 'ready', data });
    }).catch((error: unknown) => {
      if (!cancelled) setState(error instanceof ApiError && (error.status === 401 || error.status === 403) ? { status: 'permission', message: 'Your session cannot read this run event stream.' } : { status: 'error', message: apiErrorMessage(error) });
    });
    return () => { cancelled = true; };
  }, [api, organization, project, refreshKey, run.id]);
  if (state.status !== 'ready') return <SectionCard title="Event timeline" eyebrow="Live run events" className="events-card"><StatePanel state={state} /></SectionCard>;
  return <SectionCard title="Event timeline" eyebrow={`Live event history · ${run.id}`} action={<span className="stream-state"><span className="online-dot" /> API stream</span>} className="events-card"><div className="event-list">{state.data.map((event, index) => <div className="event-row" key={event.sequence}><div className="event-rail"><span className={`event-dot event-${event.type.split('.')[0]}`} />{index < state.data.length - 1 ? <span className="event-line" /> : null}</div><div className="event-content"><div className="event-meta"><strong>{event.type}</strong><span>{formatTime(event.createdAt)}</span><span className="mono-text">#{event.sequence}</span></div><p>{eventSummary(event)}</p></div></div>)}</div></SectionCard>;
}

function LiveRunDetail({ api, liveMode, organization, project, refreshKey, run }: LiveViewScope & { run: Run }) {
  return <div className="run-detail-stack"><SectionCard title="Run detail" eyebrow="Live control-plane state"><div className="run-detail-head"><div className={`run-type large ${run.kind === 'WorkflowRun' ? 'workflow' : 'agent'}`}>{run.kind === 'WorkflowRun' ? <Workflow size={20} /> : <Bot size={20} />}</div><div><h3>{run.name}</h3><p className="mono-text">{run.id}</p></div><StatusPill status={run.status} /></div><div className="run-facts"><Fact label="Definition" value={run.definition} /><Fact label="Owner" value={run.owner} /><Fact label="Started" value={run.started} /><Fact label="Duration" value={run.duration} mono /></div></SectionCard><LiveEventsPanel api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} run={run} /></div>;
}

function LiveRunsView({ api, liveMode, organization, project, refreshKey }: LiveViewScope) {
  const state = useLiveRuns(api, liveMode, organization, project, refreshKey);
  const [selected, setSelected] = useState<Run>();
  useEffect(() => { if (state.status === 'ready') setSelected(state.data[0]); }, [state]);
  if (state.status !== 'ready') return <StatePanel state={state} />;
  if (state.data.length === 0) return <StatePanel state={{ status: 'empty', message: 'No runs were returned by the control plane.' }} />;
  return <div className="run-layout"><SectionCard title="Runs" eyebrow="Live project collection · newest first" action={<div className="header-actions"><button className="secondary-button" onClick={() => window.location.reload()}><RefreshCw size={15} /> Refresh</button></div>} className="run-list-card"><div className="filter-row"><div className="segmented"><button className="selected">All <span>{state.data.length}</span></button><button>Running <span>{state.data.filter((run) => run.status === 'Running').length}</span></button><button>Waiting <span>{state.data.filter((run) => run.status === 'WaitingApproval').length}</span></button><button>Completed</button></div><button className="icon-button" aria-label="Filter runs"><ListChecks size={17} /></button></div><RunTable data={state.data} onSelect={setSelected} /></SectionCard>{selected ? <LiveRunDetail api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} run={selected} /> : null}</div>;
}

function DemoRunsView({ onLiveMode }: { onLiveMode: (run: Run) => void }) {
  const [selected, setSelected] = useState(runs[0]);
  return <div className="run-layout"><SectionCard title="Runs" eyebrow="All projects · newest first" action={<div className="header-actions"><button className="secondary-button"><RefreshCw size={15} /> Refresh</button><button className="primary-button"><Plus size={15} /> Run agent</button></div>} className="run-list-card"><div className="filter-row"><div className="segmented"><button className="selected">All <span>12</span></button><button>Running <span>4</span></button><button>Waiting <span>3</span></button><button>Completed</button></div><button className="icon-button" aria-label="Filter runs"><ListChecks size={17} /></button></div><RunTable onSelect={(run) => { setSelected(run); onLiveMode(run); }} /></SectionCard><RunDetail run={selected} /></div>;
}

function RunsView({ api, liveMode, organization, project, refreshKey, onLiveMode }: LiveViewScope & { onLiveMode: (run: Run) => void }) {
  return liveMode
    ? <LiveRunsView api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} />
    : <DemoRunsView onLiveMode={onLiveMode} />;
}

function RunDetail({ run }: { run: Run }) { return <div className="run-detail-stack"><SectionCard title="Run detail" eyebrow="Demo execution"><div className="run-detail-head"><div className={`run-type large ${run.kind === 'WorkflowRun' ? 'workflow' : 'agent'}`}>{run.kind === 'WorkflowRun' ? <Workflow size={20} /> : <Bot size={20} />}</div><div><h3>{run.name}</h3><p className="mono-text">{run.id}</p></div><StatusPill status={run.status} /><button className="icon-button" aria-label="More run actions"><MoreHorizontal size={17} /></button></div><div className="run-facts"><Fact label="Definition" value={run.definition} /><Fact label="Owner" value={run.owner} /><Fact label="Started" value={run.started} /><Fact label="Duration" value={run.duration} mono /></div><DAG /></SectionCard><EventsPanel /></div>; }
function DAG() { const nodes = [{ label: 'Investigate', status: 'Succeeded', icon: Search }, { label: 'Implement', status: 'Running', icon: Code2 }, { label: 'Review', status: 'Pending', icon: FileCheck2 }]; return <div className="dag"><div className="dag-label">Workflow progress</div><div className="dag-flow">{nodes.map((node, index) => <div className="dag-wrap" key={node.label}><div className={`dag-node dag-${node.status.toLowerCase()}`}><span><node.icon size={15} /></span><div><strong>{node.label}</strong><small>{node.status}</small></div></div>{index < nodes.length - 1 ? <div className={`dag-line ${index === 0 ? 'complete' : ''}`} /> : null}</div>)}</div></div>; }

function EventsPanel() { const [visibleEvents, setVisibleEvents] = useState(events); return <SectionCard title="Event timeline" eyebrow="Demo SSE stream · run-fix-issue-0842" action={<span className="stream-state"><span className="online-dot" /> Demo stream</span>} className="events-card"><div className="event-list">{visibleEvents.map((event) => <div className="event-row" key={event.sequence}><div className="event-rail"><span className={`event-dot event-${event.type.split('.')[0]}`} />{event.sequence !== visibleEvents[visibleEvents.length - 1].sequence ? <span className="event-line" /> : null}</div><div className="event-content"><div className="event-meta"><strong>{event.type}</strong><span>{formatTime(event.createdAt)}</span><span className="mono-text">#{event.sequence}</span></div><p>{eventSummary(event)}</p></div></div>)}</div><button className="load-more" onClick={() => setVisibleEvents((current) => current.length === events.length ? current : [...current, ...events.slice(current.length)])}>{visibleEvents.length === events.length ? 'All events loaded' : 'Load more events'}</button></SectionCard>; }
function eventSummary(event: ApiEvent) { const values = Object.entries(event.payload).filter(([key]) => key !== 'preview').map(([key, value]) => `${key}: ${String(value)}`); return event.payload.preview ? String(event.payload.preview) : values.join(' · '); }

function approvalFromResource(resource: ApiResource<Record<string, unknown>>): LiveApproval | null {
  const document = resource.document;
  const spec = (document.spec ?? {}) as Record<string, unknown>;
  const status = (document.status ?? {}) as Record<string, unknown>;
  const role = String(spec.role ?? '');
  const decision = String(status.decision ?? '').toLowerCase();
  if (decision && decision !== 'pending') return null;
  return {
    id: resource.name,
    title: String(spec.reason ?? resource.name),
    context: `Approval for run ${String(spec.runRef ?? 'unknown run')}`,
    requester: String(spec.runRef ?? 'unknown run'),
    requested: resource.createdAt ? formatTime(resource.createdAt) : 'Unknown',
    risk: role.toLowerCase().includes('admin') ? 'High' : role ? 'Medium' : 'Low',
    runID: String(spec.runRef ?? ''),
    stepID: String(spec.stepId ?? resource.name),
  };
}

function DemoApprovalsView() {
  const [pending, setPending] = useState(approvals);
  const decide = (id: string) => setPending((current) => current.filter((approval) => approval.id !== id));
  return <SectionCard title="Approval queue" eyebrow={`${pending.length} pending decisions`} action={<div className="approval-policy"><ShieldCheck size={15} /> Write effects require approval</div>}><div className="approval-list">{pending.map((approval) => <div className="approval-card" key={approval.id}><div className={`approval-risk risk-${approval.risk.toLowerCase()}`}><ShieldCheck size={17} /></div><div className="approval-main"><div className="approval-title"><h3>{approval.title}</h3><span className={`risk-label risk-${approval.risk.toLowerCase()}`}>{approval.risk} risk</span></div><p>{approval.context}</p><div className="approval-meta"><span><PlayCircle size={13} /> {approval.requester}</span><span><Clock3 size={13} /> {approval.requested}</span><span className="mono-text">{approval.id}</span></div></div><div className="approval-actions"><button className="secondary-button" onClick={() => decide(approval.id)}>Deny</button><button className="primary-button" onClick={() => decide(approval.id)}><Check size={15} /> Approve</button></div></div>)}</div>{pending.length === 0 ? <StatePanel state={{ status: 'empty' }} /> : null}</SectionCard>;
}

function LiveApprovalsView({ api, liveMode, organization, project, refreshKey }: LiveViewScope) {
  const state = useLiveCollection(liveMode, refreshKey, () => api.listApprovals(organization, project), 'No pending approval resources were returned.');
  const [pending, setPending] = useState<LiveApproval[]>([]);
  const [actionError, setActionError] = useState('');
  const [busyID, setBusyID] = useState('');
  useEffect(() => {
    if (state.status === 'ready') {
      setPending(state.data.map(approvalFromResource).filter((approval): approval is LiveApproval => approval !== null && Boolean(approval.runID)));
    }
  }, [state]);
  if (state.status !== 'ready') return <StatePanel state={state} />;
  const decide = async (approval: LiveApproval, decision: 'approved' | 'denied') => {
    setBusyID(approval.id);
    setActionError('');
    try {
      await api.submitApproval(organization, project, approval.runID, { target: 'workflow', approvalId: approval.id, stepId: approval.stepID, decision }, `console-${approval.id}-${decision}`);
      setPending((current) => current.filter((item) => item.id !== approval.id));
    } catch (error: unknown) {
      setActionError(apiErrorMessage(error));
    } finally {
      setBusyID('');
    }
  };
  return <SectionCard title="Approval queue" eyebrow={`${pending.length} pending decisions · live resources`} action={<div className="approval-policy"><ShieldCheck size={15} /> Write effects require approval</div>}>{actionError ? <div className="state-panel state-error"><CircleAlert size={18} /><strong>Decision failed</strong><span>{actionError}</span></div> : null}<div className="approval-list">{pending.map((approval) => <div className="approval-card" key={approval.id}><div className={`approval-risk risk-${approval.risk.toLowerCase()}`}><ShieldCheck size={17} /></div><div className="approval-main"><div className="approval-title"><h3>{approval.title}</h3><span className={`risk-label risk-${approval.risk.toLowerCase()}`}>{approval.risk} risk</span></div><p>{approval.context}</p><div className="approval-meta"><span><PlayCircle size={13} /> {approval.requester}</span><span><Clock3 size={13} /> {approval.requested}</span><span className="mono-text">{approval.id}</span></div></div><div className="approval-actions"><button className="secondary-button" disabled={busyID === approval.id} onClick={() => void decide(approval, 'denied')}>Deny</button><button className="primary-button" disabled={busyID === approval.id} onClick={() => void decide(approval, 'approved')}><Check size={15} /> Approve</button></div></div>)}</div>{pending.length === 0 ? <StatePanel state={{ status: 'empty', message: 'No pending approval resources were returned.' }} /> : null}</SectionCard>;
}

function ApprovalsView({ api, liveMode, organization, project, refreshKey }: LiveViewScope) {
  return liveMode
    ? <LiveApprovalsView api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} />
    : <DemoApprovalsView />;
}

function DemoEntitlementsView() { return <div className="two-column-view"><SectionCard title="Model routes" eyebrow="Provider selection policy" action={<button className="secondary-button"><Plus size={15} /> Add route</button>}><div className="route-list"><RouteCard name="coding-default" status="Healthy" providers={[['Codex subscription', 'gpt-5-codex', 'Owner · Soham'], ['OpenAI API', 'gpt-5-codex', 'Fallback · org']]} /><RouteCard name="general-default" status="Healthy" providers={[['Claude subscription', 'claude-sonnet-4-5', 'Owner · Soham'], ['OpenRouter', 'auto', 'Fallback · org']]} /><RouteCard name="local-fast" status="Paused" providers={[['Ollama', 'qwen3:8b', 'sohim-home']]} /></div></SectionCard><SectionCard title="Entitlements" eyebrow="Credential boundaries"><div className="entitlement-list"><EntitlementRow icon={UserRound} name="Soham · Codex subscription" detail="Owner-bound · reset in 2h 14m" status="Available" /><EntitlementRow icon={UserRound} name="Soham · Claude subscription" detail="Owner-bound · reset in 4h 02m" status="Available" /><EntitlementRow icon={KeyRound} name="Astatide · OpenAI API" detail="Organization credential · $18.42 / $100" status="Available" /><EntitlementRow icon={Network} name="Astatide · OpenRouter" detail="Organization credential · fallback" status="Available" /></div></SectionCard></div>; }

function routeFromResource(resource: ApiResource<Record<string, unknown>>) {
  const spec = (resource.document.spec ?? {}) as Record<string, unknown>;
  const providers = Array.isArray(spec.providers) ? spec.providers.flatMap((provider) => {
    if (!provider || typeof provider !== 'object') return [];
    const item = provider as Record<string, unknown>;
    return [[String(item.name ?? item.kind ?? 'Provider'), String(item.model ?? 'unspecified'), `${String(item.kind ?? 'provider')}${item.fallback ? ' · fallback' : ' · primary'}`]];
  }) : [];
  return { name: resource.name, status: 'Configured', providers };
}

function entitlementFromResource(resource: ApiResource<Record<string, unknown>>) {
  const spec = (resource.document.spec ?? {}) as Record<string, unknown>;
  const status = (resource.document.status ?? {}) as Record<string, unknown>;
  const available = status.available === true;
  const cooldown = status.cooldownUntil ? ` · cooldown until ${String(status.cooldownUntil)}` : '';
  return { name: resource.name, detail: `${String(spec.owner ?? 'Unknown owner')} · ${String(spec.provider ?? 'Unknown provider')}${cooldown}`, status: available ? 'Available' : 'Unavailable' };
}

function LiveEntitlementsView({ api, liveMode, organization, project, refreshKey }: LiveViewScope) {
  const routes = useLiveCollection(liveMode, refreshKey, () => api.listModelRoutes(organization, project), 'No model routes have been configured.');
  const entitlements = useLiveCollection(liveMode, refreshKey, () => api.listEntitlements(organization, project), 'No model entitlements have been configured.');
  return <div className="two-column-view"><SectionCard title="Model routes" eyebrow="Live provider selection policy">{routes.status === 'ready' ? <div className="route-list">{routes.data.map((resource) => { const route = routeFromResource(resource); return <RouteCard key={resource.name} {...route} />; })}</div> : <StatePanel state={routes} />}</SectionCard><SectionCard title="Entitlements" eyebrow="Live credential boundaries"><div className="entitlement-list">{entitlements.status === 'ready' ? entitlements.data.map((resource) => { const entitlement = entitlementFromResource(resource); return <EntitlementRow key={resource.name} icon={entitlement.status === 'Available' ? UserRound : Network} {...entitlement} />; }) : <StatePanel state={entitlements} />}</div></SectionCard></div>;
}

function EntitlementsView({ api, liveMode, organization, project, refreshKey }: LiveViewScope) {
  return liveMode
    ? <LiveEntitlementsView api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} />
    : <DemoEntitlementsView />;
}
function RouteCard({ name, status, providers }: { name: string; status: string; providers: string[][] }) { return <div className="route-card"><div className="route-head"><div><strong>{name}</strong><small>Explicit provider order</small></div><StatusPill status={status} /></div>{providers.map(([provider, model, detail], index) => <div className="provider-row" key={provider}><span className="provider-number">{index + 1}</span><span className="provider-logo">{provider[0]}</span><div><strong>{provider}</strong><small>{model} · {detail}</small></div>{index === 0 ? <span className="primary-label">Primary</span> : <span className="muted-text">Fallback</span>}</div>)}</div>; }
function EntitlementRow({ icon: Icon, name, detail, status }: { icon: LucideIcon; name: string; detail: string; status: string }) { return <div className="entitlement-row"><span className="entitlement-icon"><Icon size={16} /></span><div><strong>{name}</strong><small>{detail}</small></div><StatusPill status={status} /></div>; }

function QuotaCard({ label, value, percent, detail, tone }: { label: string; value: string; percent: number; detail: string; tone: string }) { return <div className="quota-card"><div className="quota-card-head"><span>{label}</span><span className={`quota-dot ${tone}`} /></div><strong>{value}</strong><div className="progress"><span className={tone} style={{ width: `${percent}%` }} /></div><small>{detail}</small></div>; }
function QuotaRow({ resource, current, limit, percent, policy }: { resource: string; current: string; limit: string; percent: number; policy: string }) { return <tr><td><strong>{resource}</strong></td><td className="mono-text">{current}</td><td className="muted-text">{limit}</td><td><div className="table-progress"><span><i style={{ width: `${percent}%` }} /></span><strong>{percent}%</strong></div></td><td><span className="muted-text">{policy}</span></td></tr>; }

interface LiveQuotaData { quotas: Record<string, unknown>; usage: ApiUsage; warnings: string[] }

function quotaNumber(quotas: Record<string, unknown>, key: string): number | undefined {
  const value = quotas[key];
  return typeof value === 'number' && Number.isFinite(value) && value > 0 ? value : undefined;
}

function parseStorageBytes(value: unknown): number | undefined {
  if (typeof value !== 'string') return undefined;
  const match = value.trim().match(/^([\d.]+)\s*(kb|mb|gb|tb)?$/i);
  if (!match) return undefined;
  const amount = Number(match[1]);
  const unit = (match[2] ?? 'b').toLowerCase();
  const multipliers: Record<string, number> = { b: 1, kb: 1024, mb: 1024 ** 2, gb: 1024 ** 3, tb: 1024 ** 4 };
  return Number.isFinite(amount) ? amount * (multipliers[unit] ?? 1) : undefined;
}

function formatBytes(value: number): string {
  if (value < 1024 ** 2) return `${Math.round(value / 1024)} KB`;
  if (value < 1024 ** 3) return `${(value / 1024 ** 2).toFixed(1)} MB`;
  return `${(value / 1024 ** 3).toFixed(1)} GB`;
}

function percent(current: number | undefined, limit: number | undefined): number {
  return current !== undefined && limit && limit > 0 ? Math.min(100, Math.round((current / limit) * 100)) : 0;
}

function useLiveQuotas({ api, liveMode, organization, project, refreshKey }: LiveViewScope) {
  const [state, setState] = useState<RemoteState<LiveQuotaData>>({ status: 'loading' });
  useEffect(() => {
    let cancelled = false;
    if (!liveMode) return () => { cancelled = true; };
    setState({ status: 'loading' });
    const quotasPromise = api.getQuotas(organization, project);
    const usagePromise = api.getUsage(organization, project);
    Promise.allSettled([quotasPromise, usagePromise]).then(([quotasResult, usageResult]) => {
      if (cancelled) return;
      if (quotasResult.status === 'rejected') {
        setState(apiStateFromErrors([quotasResult.reason], 'The project quota collection is unavailable.'));
        return;
      }
      const warnings: string[] = [];
      const usage = usageResult.status === 'fulfilled' ? usageResult.value : quotasResult.value.usage;
      if (usageResult.status === 'rejected') warnings.push('The dedicated usage endpoint could not be read; showing usage returned with quotas.');
      setState({ status: 'ready', data: { quotas: quotasResult.value.quotas, usage, warnings } });
    });
    return () => { cancelled = true; };
  }, [api, liveMode, organization, project, refreshKey]);
  return state;
}

function LiveQuotasView({ api, liveMode, organization, project, refreshKey }: LiveViewScope) {
  const state = useLiveQuotas({ api, liveMode, organization, project, refreshKey });
  if (state.status !== 'ready') return <StatePanel state={state} />;
  const { quotas, usage, warnings } = state.data;
  const { activeRuns, artifactBytes, artifactVersions: artifactCount } = usage;
  const concurrent = quotaNumber(quotas, 'concurrentRuns');
  const storageLimit = parseStorageBytes(quotas.storage);
  const cards = [
    { label: 'Concurrent runs', value: `${activeRuns} / ${concurrent ?? '—'}`, percent: percent(activeRuns, concurrent), detail: concurrent ? `${Math.max(0, concurrent - activeRuns)} slots available` : 'Usage limit not configured', tone: 'blue' },
    { label: 'Model spend', value: `— / ${quotaNumber(quotas, 'modelSpendUsd') ? `$${Number(quotas.modelSpendUsd).toFixed(2)}` : '—'}`, percent: 0, detail: 'Spend accounting is not reported by the usage contract', tone: 'green' },
    { label: 'Artifact storage', value: `${formatBytes(artifactBytes)} / ${String(quotas.storage ?? '—')}`, percent: percent(artifactBytes, storageLimit), detail: `${artifactCount} catalog versions · ${storageLimit ? formatBytes(Math.max(0, storageLimit - artifactBytes)) + ' available' : 'limit not configured'}`, tone: 'purple' },
  ];
  const hasQuota = Object.keys(quotas).length > 0;
  return <div className="quotas-stack">{warnings.map((warning) => <div className="state-panel" key={warning}><AlertTriangle size={18} /><strong>Partial usage</strong><span>{warning}</span></div>)}{!hasQuota ? <StatePanel state={{ status: 'empty', message: 'The live project resource has no quota limits configured.' }} /> : <><div className="quota-overview">{cards.map((card) => <QuotaCard key={card.label} {...card} />)}</div><SectionCard title="Project quotas" eyebrow={`${organization} / ${project} · live project resource`}><div className="table-wrap"><table><caption className="sr-only">Project quotas</caption><thead><tr><th>Resource</th><th>Current</th><th>Limit</th><th>Utilization</th><th>Policy</th></tr></thead><tbody><QuotaRow resource="Concurrent runs" current={String(activeRuns)} limit={concurrent ? String(concurrent) : '—'} percent={percent(activeRuns, concurrent)} policy="Configured resource" /><QuotaRow resource="CPU allocation" current="—" limit={String(quotas.cpu ?? '—')} percent={0} policy="Usage endpoint required" /><QuotaRow resource="Memory allocation" current="—" limit={String(quotas.memory ?? '—')} percent={0} policy="Usage endpoint required" /><QuotaRow resource="Monthly model spend" current="—" limit={quotaNumber(quotas, 'modelSpendUsd') ? `$${Number(quotas.modelSpendUsd).toFixed(2)}` : '—'} percent={0} policy="Accounting endpoint required" /></tbody></table></div></SectionCard></>}</div>;
}

function DemoQuotasView() { return <div className="quotas-stack"><div className="quota-overview"><QuotaCard label="Concurrent runs" value="4 / 12" percent={33} detail="8 slots available" tone="blue" /><QuotaCard label="Model spend" value="$18.42 / $100" percent={18} detail="$81.58 remaining this month" tone="green" /><QuotaCard label="Artifact storage" value="3.2 / 50 GB" percent={6} detail="47 GB available" tone="purple" /></div><SectionCard title="Project quotas" eyebrow="Astatide / agents-gateway"><div className="table-wrap"><table><caption className="sr-only">Project quotas</caption><thead><tr><th>Resource</th><th>Current</th><th>Limit</th><th>Utilization</th><th>Policy</th></tr></thead><tbody><QuotaRow resource="Concurrent runs" current="4" limit="12" percent={33} policy="Queue" /><QuotaRow resource="CPU allocation" current="6.4 vCPU" limit="24 vCPU" percent={27} policy="Queue" /><QuotaRow resource="Memory allocation" current="11.2 GB" limit="48 GB" percent={23} policy="Queue" /><QuotaRow resource="Monthly model spend" current="$18.42" limit="$100.00" percent={18} policy="Wait for approval" /></tbody></table></div></SectionCard></div>; }

function QuotasView({ api, liveMode, organization, project, refreshKey }: LiveViewScope) {
  return liveMode
    ? <LiveQuotasView api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} />
    : <DemoQuotasView />;
}

function DemoAuditView() { return <SectionCard title="Audit history" eyebrow="Append-only control-plane record" action={<div className="filter-row compact-filter"><button className="secondary-button"><FileCheck2 size={15} /> Export JSONL</button><button className="icon-button" aria-label="Filter audit history"><ListChecks size={17} /></button></div>}><div className="audit-callout"><LockKeyhole size={17} /><div><strong>Audit integrity enabled</strong><span>Events are written before successful responses and retained for 180 days.</span></div><span className="mono-text">chain: verified</span></div><div className="table-wrap"><table><caption className="sr-only">Audit history</caption><thead><tr><th>Action</th><th>Subject</th><th>Actor</th><th>Time</th><th>Decision</th></tr></thead><tbody>{auditEntries.map((entry) => <tr key={`${entry.action}-${entry.subject}`}><td><span className="audit-action"><span className={`audit-dot ${entry.result.toLowerCase()}`} />{entry.action}</span></td><td className="mono-text">{entry.subject}</td><td>{entry.actor}</td><td className="muted-text">{entry.time}</td><td><StatusPill status={entry.result} /></td></tr>)}</tbody></table></div></SectionCard>; }

function LiveAuditView({ api, liveMode, organization, project, refreshKey }: LiveViewScope) {
  const state = useLiveCollection(liveMode, refreshKey, () => api.listAudit(organization, project), 'No audit entries have been recorded for this project yet.');
  if (state.status !== 'ready') return <SectionCard title="Audit history" eyebrow="Live control-plane record"><StatePanel state={state} /></SectionCard>;
  return <SectionCard title="Audit history" eyebrow="Append-only control-plane record" action={<div className="filter-row compact-filter"><button className="secondary-button"><FileCheck2 size={15} /> Export JSONL</button><button className="icon-button" aria-label="Filter audit history"><ListChecks size={17} /></button></div>}><div className="audit-callout"><LockKeyhole size={17} /><div><strong>Audit integrity enabled</strong><span>Live entries are read from the authenticated audit collection.</span></div><span className="mono-text">chain: server-managed</span></div><div className="table-wrap"><table><caption className="sr-only">Audit history</caption><thead><tr><th>Action</th><th>Subject</th><th>Actor</th><th>Time</th><th>Decision</th></tr></thead><tbody>{state.data.map((entry) => <tr key={`${entry.createdAt}-${entry.action}-${entry.resourceId}`}><td><span className="audit-action"><span className={`audit-dot ${entry.decision.toLowerCase()}`} />{entry.action}</span></td><td className="mono-text">{entry.resourceType}/{entry.resourceId}</td><td>{entry.principalId}</td><td className="muted-text">{formatTime(entry.createdAt)}</td><td><StatusPill status={entry.decision} /></td></tr>)}</tbody></table></div></SectionCard>;
}

function AuditView({ api, liveMode, organization, project, refreshKey }: LiveViewScope) {
  return liveMode ? <LiveAuditView api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} /> : <DemoAuditView />;
}

function DemoRunnersView() { return <div className="runners-stack"><div className="runner-health-banner"><div className="runner-health-icon"><ShieldCheck size={22} /></div><div><strong>Demo runner fleet</strong><p>Illustrative runner data; isolation and health are not verified here.</p></div><div className="runner-count"><strong>Demo 3 / 3</strong><span>online</span></div></div><SectionCard title="Execution runners" eyebrow="Demo data · illustrative heartbeats" action={<button className="secondary-button"><Plus size={15} /> Enroll runner</button>}><div className="runner-list"><RunnerRow name="runner-ovh-01" host="ovh-prod · 8 vCPU · 32 GB" backend="containerd + gVisor" grade="Enhanced" load="42%" /><RunnerRow name="runner-sohim-home" host="sohim · 4 vCPU · 16 GB" backend="rootless Podman" grade="Standard" load="68%" /><RunnerRow name="runner-ovh-02" host="ovh-preview · 4 vCPU · 16 GB" backend="containerd + gVisor" grade="Enhanced" load="18%" /></div></SectionCard></div>; }

function liveRunnerFromResource(resource: ApiResource<Record<string, unknown>>) {
  const document = resource.document;
  const spec = (document.spec ?? {}) as Record<string, unknown>;
  const status = (document.status ?? {}) as Record<string, unknown>;
  const labels = (spec.labels ?? {}) as Record<string, unknown>;
  return {
    name: resource.name,
    host: String(labels.host ?? spec.endpoint ?? 'Configured endpoint'),
    backend: Array.isArray(spec.backends) ? spec.backends.join(' + ') : 'Unknown backend',
    grade: String(status.isolation ?? 'Unknown'),
    load: '—',
    ready: status.ready === true,
    heartbeat: status.lastHeartbeat ? String(status.lastHeartbeat) : 'No heartbeat reported',
  };
}

function LiveRunnersView({ api, liveMode, organization, project, refreshKey }: LiveViewScope) {
  const state = useLiveCollection(liveMode, refreshKey, () => api.listRunners(organization, project), 'No runners have been enrolled for this project yet.');
  if (state.status !== 'ready') return <StatePanel state={state} />;
  const liveRunners = state.data.map(liveRunnerFromResource);
  const readyCount = liveRunners.filter((runner) => runner.ready).length;
  return <div className="runners-stack"><div className="runner-health-banner"><div className="runner-health-icon"><ShieldCheck size={22} /></div><div><strong>Live runner fleet</strong><p>Readiness and heartbeat fields come from configured Runner resources.</p></div><div className="runner-count"><strong>{readyCount} / {liveRunners.length}</strong><span>ready</span></div></div><SectionCard title="Execution runners" eyebrow="Live runner resources"><div className="runner-list">{liveRunners.map((runner) => <div className="runner-row" key={runner.name}><div className="runner-status"><span className={runner.ready ? 'online-dot' : 'offline-dot'} /><div><strong>{runner.name}</strong><small>{runner.host}</small></div></div><div className="runner-detail"><span><Database size={14} /> {runner.backend}</span><span><LockKeyhole size={14} /> {runner.grade} isolation</span></div><div className="runner-load"><small>Heartbeat</small><strong>{runner.heartbeat}</strong></div><StatusPill status={runner.ready ? 'Ready' : 'Not ready'} /></div>)}</div></SectionCard></div>;
}

function RunnersView({ api, liveMode, organization, project, refreshKey }: LiveViewScope) {
  return liveMode
    ? <LiveRunnersView api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} />
    : <DemoRunnersView />;
}
function RunnerRow({ name, host, backend, grade, load }: { name: string; host: string; backend: string; grade: string; load: string }) { return <div className="runner-row"><div className="runner-status"><span className="online-dot" /><div><strong>{name}</strong><small>{host}</small></div></div><div className="runner-detail"><span><Database size={14} /> {backend}</span><span><LockKeyhole size={14} /> {grade} isolation</span></div><div className="runner-load"><small>Load</small><strong>{load}</strong><span className="mini-progress"><i style={{ width: load }} /></span></div><button className="icon-button" aria-label={`Open ${name}`}><ArrowUpRight size={15} /></button></div>; }

function DemoDataNotice() {
  return <aside className="demo-notice" role="note" aria-label="Demo data notice"><AlertTriangle size={17} /><div><strong>Demo data</strong><span>This console is in demo mode. The panels below are illustrative and are not live operational status.</span></div></aside>;
}

function OwnerConnectionScreen({ organization, project, onConnect }: { organization: string; project: string; onConnect: (token: string) => void }) {
  const [draftToken, setDraftToken] = useState('');
  const [showToken, setShowToken] = useState(false);
  const [error, setError] = useState<string>();

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const token = draftToken.trim();
    if (!token) {
      setError('Enter an owner token to connect.');
      return;
    }
    setError(undefined);
    setDraftToken('');
    onConnect(token);
  };

  return <main className="connection-shell"><section className="connection-card" aria-labelledby="connection-title"><div className="connection-brand"><div className="brand-mark"><Sparkles size={18} /></div><div><strong>Agents Gateway</strong><span>Owner console</span></div></div><div className="connection-icon"><LockKeyhole size={24} /></div><div className="connection-copy"><div className="eyebrow"><span className="eyebrow-dot" />Local owner authentication</div><h1 id="connection-title">Connect to your gateway</h1><p>Enter an owner token to open the live control plane for this browser tab.</p></div><form onSubmit={submit} className="connection-form"><label htmlFor="owner-token">Owner token</label><div className="token-input-wrap"><input id="owner-token" name="owner-token" type={showToken ? 'text' : 'password'} autoComplete="off" autoCapitalize="none" spellCheck={false} value={draftToken} onChange={(event) => { setDraftToken(event.target.value); setError(undefined); }} placeholder="Paste a runtime owner token" aria-invalid={Boolean(error)} aria-describedby={error ? 'owner-token-error' : 'owner-token-help'} /><button type="button" className="token-visibility" onClick={() => setShowToken((current) => !current)} aria-label={showToken ? 'Hide owner token' : 'Show owner token'}>{showToken ? 'Hide' : 'Show'}</button></div>{error ? <span id="owner-token-error" className="connection-error" role="alert">{error}</span> : <span id="owner-token-help" className="connection-help">The token is used only in memory and is cleared when you disconnect or close this tab.</span>}<button className="primary-button connection-submit" type="submit" disabled={!draftToken.trim()}><LockKeyhole size={15} /> Connect securely</button></form><dl className="connection-scope"><div><dt>Organization</dt><dd>{organization}</dd></div><div><dt>Project</dt><dd>{project}</dd></div></dl><p className="connection-warning"><ShieldCheck size={15} /> This console never stores the token in URLs, browser storage, cookies, or build-time configuration.</p></section></main>;
}

function App() {
  const [activeView, setActiveView] = useState<ViewId>(() => (window.location.hash.slice(1) as ViewId) || 'overview');
  const [search, setSearch] = useState('');
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [refreshKey, setRefreshKey] = useState(0);
  const [controlPlane, setControlPlane] = useState<RemoteState<{ status: string }>>({ status: 'loading' });
	const runtimeTokenRef = useRef<string | null>(null);
	const [connected, setConnected] = useState(false);
	const api = useMemo(() => createApiClient({ tokenProvider: () => runtimeTokenRef.current }), []);
	const liveMode = import.meta.env.VITE_AGW_MODE !== 'demo';
	const organization = import.meta.env.VITE_AGW_ORGANIZATION?.trim() || DEFAULT_ORGANIZATION;
	const project = import.meta.env.VITE_AGW_PROJECT?.trim() || DEFAULT_PROJECT;

  useEffect(() => { if (!navTitle[activeView]) setActiveView('overview'); window.history.replaceState({}, '', `#${activeView}`); setSidebarOpen(false); }, [activeView]);
  useEffect(() => {
    let cancelled = false;
    if (!liveMode) { setControlPlane({ status: 'ready', data: { status: 'demo' } }); return () => { cancelled = true; }; }
    setControlPlane({ status: 'loading' });
    api.ready().then((data) => { if (!cancelled) setControlPlane({ status: 'ready', data }); }).catch((error: unknown) => {
      if (cancelled) return;
      if (error instanceof ApiError && (error.status === 401 || error.status === 403)) setControlPlane({ status: 'permission', message: 'Your session is not authorized for this organization or project.' });
      else setControlPlane({ status: 'error', message: error instanceof Error ? error.message : 'Could not reach the API.' });
    });
    return () => { cancelled = true; };
  }, [api, liveMode, connected]);

  const navigate = (view: ViewId) => setActiveView(view);
  const connect = (token: string) => {
    runtimeTokenRef.current = token;
    setConnected(true);
    setRefreshKey((current) => current + 1);
  };
  const disconnect = () => {
    runtimeTokenRef.current = null;
    setConnected(false);
    setRefreshKey((current) => current + 1);
  };
  const currentView = activeView;
	if (liveMode && !connected) return <OwnerConnectionScreen organization={organization} project={project} onConnect={connect} />;
	return <div className="app-shell"><div className={`sidebar-shell ${sidebarOpen ? 'open' : ''}`}><Sidebar active={currentView} onNavigate={navigate} liveMode={liveMode} organization={organization} project={project} onDisconnect={liveMode ? disconnect : undefined} /></div>{sidebarOpen ? <button className="sidebar-scrim" aria-label="Close navigation" onClick={() => setSidebarOpen(false)} /> : null}<main className="main-content"><TopBar onMenu={() => setSidebarOpen(true)} onRefresh={() => setRefreshKey((current) => current + 1)} search={search} setSearch={setSearch} liveMode={liveMode} organization={organization} project={project} /><div className="content-wrap"><PageHeader view={currentView} onRun={() => navigate('runs')} />{controlPlane.status !== 'ready' ? <StatePanel state={controlPlane} /> : null}<div className="mode-note"><span className={`mode-dot ${liveMode ? 'live' : 'demo'}`} /><span>{liveMode ? currentView === 'artifacts' ? 'Live artifact catalog' : 'Live API-backed panel' : 'Demo mode'}</span><span className="mode-separator">·</span><span>{liveMode ? 'Authenticated project collections are live' : 'Changes are managed in Git and the CLI'}</span>{!liveMode ? <button className="text-button" onClick={() => navigate('definitions')}>View definitions <ArrowUpRight size={13} /></button> : null}</div>{!liveMode ? <DemoDataNotice /> : null}{currentView === 'overview' ? <Overview api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} onView={navigate} readiness={controlPlane} /> : currentView === 'definitions' ? <DefinitionsView api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} /> : currentView === 'runs' ? <RunsView api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} onLiveMode={() => undefined} /> : currentView === 'approvals' ? <ApprovalsView api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} /> : currentView === 'artifacts' ? <ArtifactsView api={api} liveMode={liveMode} organization={organization} project={project} /> : currentView === 'entitlements' ? <EntitlementsView api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} /> : currentView === 'quotas' ? <QuotasView api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} /> : currentView === 'audit' ? <AuditView api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} /> : <RunnersView api={api} liveMode={liveMode} organization={organization} project={project} refreshKey={refreshKey} />}</div></main></div>;
}

export default App;
