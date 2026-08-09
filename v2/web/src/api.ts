export type ResourceKind =
  | 'Organization'
  | 'Project'
  | 'Agent'
  | 'SkillSet'
  | 'ToolSet'
  | 'SandboxProfile'
  | 'ModelRoute'
  | 'Workflow'
  | 'AgentRun'
  | 'WorkflowRun'
  | 'Approval'
  | 'Artifact'
  | 'Runner'
  | 'Credential'
  | 'Entitlement';

export type RunStatus = 'Pending' | 'Scheduled' | 'Starting' | 'Running' | 'WaitingApproval' | 'Succeeded' | 'Failed' | 'Cancelled' | 'Lost';

export interface ApiEnvelope<T> {
  data?: T;
  events?: T[];
  error?: { code?: string; message?: string };
  requestId?: string;
}

export interface ApiResource<T = Record<string, unknown>> {
  kind: ResourceKind | string;
  name: string;
  digest: string;
  revision: number;
  appliedBy?: string;
  createdAt?: string;
  document: T;
}

export interface ApiRun {
  id: string;
  kind: 'AgentRun' | 'WorkflowRun' | string;
  status: RunStatus | string;
  condition?: string;
  definitionDigest: string;
  requestedBy?: string;
  createdAt: string;
  updatedAt: string;
}

export interface ApiCollection<T> {
  items: T[];
  hasMore: boolean;
  nextOffset: number | null;
  limit: number;
  offset: number;
}

export interface ApiUsage {
  activeRuns: number;
  totalRuns: number;
  artifactVersions: number;
  artifactBytes: number;
}

export interface ApiQuotaData {
  usage: ApiUsage;
  quotas: Record<string, unknown>;
}

export interface ApiAuditEntry {
  principalId: string;
  action: string;
  resourceType: string;
  resourceId: string;
  decision: string;
  metadata?: Record<string, unknown>;
  createdAt: string;
}

export interface ApiEvent {
  sequence: number;
  type: string;
  payload: Record<string, unknown>;
  createdAt: string;
}

export interface ApiArtifactRef {
  id: string;
  uri: string;
  digest: string;
  size_bytes: number;
  media_type: string;
}

export interface ApiArtifactVersion {
  schema_version: number;
  version_id: string;
  artifact_id: string;
  created_at: string;
  lineage: { parent_version_id?: string; forked_from?: { artifact_id: string; version_id: string } };
  manifest: {
    schema_version: number;
    artifact_id: string;
    title: string;
    description?: string;
    content_kind: 'document' | 'code' | 'single_page_html' | 'svg' | 'diagram' | 'interactive_component';
    media_type: string;
    renderer: { kind: string; source_view: boolean; download: boolean };
    security: { allow?: string[] };
    preview: { alt_text?: string; summary?: string; width_px?: number; height_px?: number };
  };
  content: ApiArtifactRef;
  source: ApiArtifactRef;
}

export interface ApiArtifactDetail {
  artifactId: string;
  versions: ApiArtifactVersion[];
}

export interface ApiArtifactContent {
  body: string;
  mediaType: string;
  sizeBytes: number;
}

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId?: string;

  constructor(message: string, status: number, code = 'request_failed', requestId?: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

export type RemoteState<T> =
  | { status: 'loading' }
  | { status: 'empty'; message?: string }
  | { status: 'permission'; message: string }
  | { status: 'error'; message: string }
  | { status: 'unavailable'; message: string }
  | { status: 'ready'; data: T };

export interface ApiClientOptions {
  baseUrl?: string;
  /**
   * Runtime-only token resolution. Implementations should obtain a short-lived
   * token from an in-memory session/auth SDK; never read build-time env or
   * browser storage here. Omit this when the API uses a same-origin HttpOnly
   * session cookie.
   */
  tokenProvider?: TokenProvider;
  fetcher?: typeof fetch;
}

export type TokenProvider = () => string | null | Promise<string | null>;

export interface CreateRunInput {
  id?: string;
  kind: 'AgentRun' | 'WorkflowRun';
  definitionDigest: string;
  idempotencyKey?: string;
  agentRef?: string;
  workflowRef?: string;
  inputRef?: string;
}

export interface ApprovalSignalInput {
  target?: 'workflow' | 'agent';
  approvalId: string;
  stepId?: string;
  decision: 'approved' | 'denied';
}

export type EventListener = (event: ApiEvent) => void;

function joinUrl(base: string, path: string): string {
  return `${base.replace(/\/$/, '')}/${path.replace(/^\//, '')}`;
}

function unwrap<T>(envelope: ApiEnvelope<T>): T {
  if (envelope.data !== undefined) return envelope.data;
  return envelope as T;
}

export class ApiClient {
  private readonly baseUrl: string;
  private readonly tokenProvider?: TokenProvider;
  private readonly fetcher: typeof fetch;

  constructor(options: ApiClientOptions = {}) {
    this.baseUrl = options.baseUrl ?? '/api/v1alpha1';
    this.tokenProvider = options.tokenProvider;
    // Browser fetch is a Web-IDL method and some engines require its Window
    // receiver. Keeping a detached reference causes `Illegal invocation` in
    // production even though ordinary test doubles accept the call.
    this.fetcher = options.fetcher ?? globalThis.fetch.bind(globalThis);
  }

  private async headers(accept: string, init?: HeadersInit, includeAuth = true): Promise<Headers> {
    const headers = new Headers(init);
    headers.set('Accept', accept);
    if (includeAuth) {
      const token = await this.tokenProvider?.();
      if (token?.trim()) headers.set('Authorization', `Bearer ${token.trim()}`);
    }
    return headers;
  }

  private async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const headers = await this.headers('application/json', init.headers);
    if (init.body) headers.set('Content-Type', 'application/json');

    const response = await this.fetcher(joinUrl(this.baseUrl, path), { ...init, headers, credentials: 'same-origin' });
    const body = await response.json().catch(() => ({})) as ApiEnvelope<T>;
    if (!response.ok) {
      throw new ApiError(body.error?.message ?? `Request failed with status ${response.status}`, response.status, body.error?.code, body.requestId);
    }
    return unwrap(body);
  }

  private scope(organization: string, project: string): string {
    return `organizations/${encodeURIComponent(organization)}/projects/${encodeURIComponent(project)}`;
  }

  async health(): Promise<{ status: string }> {
    const root = this.baseUrl.replace(/\/api\/v1alpha1\/?$/, '');
    const response = await this.fetcher(joinUrl(root || '/', 'health'), { headers: await this.headers('application/json', undefined, false), credentials: 'same-origin' });
    if (!response.ok) throw new ApiError('Health check failed', response.status);
    return response.json() as Promise<{ status: string }>;
  }

  async ready(): Promise<{ status: string }> {
    const root = this.baseUrl.replace(/\/api\/v1alpha1\/?$/, '');
    const response = await this.fetcher(joinUrl(root || '/', 'ready'), { headers: await this.headers('application/json', undefined, false), credentials: 'same-origin' });
    const body = await response.json().catch(() => ({})) as ApiEnvelope<{ status: string }>;
    if (!response.ok) throw new ApiError(body.error?.message ?? 'Readiness check failed', response.status, body.error?.code, body.requestId);
    return unwrap(body);
  }

  getResource<T>(organization: string, project: string, kind: ResourceKind, name: string): Promise<ApiResource<T>> {
    return this.request<ApiResource<T>>(`${this.scope(organization, project)}/resources/${encodeURIComponent(kind)}/${encodeURIComponent(name)}`);
  }

  applyResource<T>(organization: string, project: string, kind: ResourceKind, name: string, resource: T): Promise<ApiResource<T>> {
    return this.request<ApiResource<T>>(`${this.scope(organization, project)}/resources/${encodeURIComponent(kind)}/${encodeURIComponent(name)}`, {
      method: 'PUT',
      body: JSON.stringify(resource),
    });
  }

  createRun(organization: string, project: string, input: CreateRunInput): Promise<ApiRun> {
    return this.request<ApiRun>(`${this.scope(organization, project)}/runs`, {
      method: 'POST',
      body: JSON.stringify(input),
    });
  }

  getRun(organization: string, project: string, runId: string): Promise<ApiRun> {
    return this.request<ApiRun>(`${this.scope(organization, project)}/runs/${encodeURIComponent(runId)}`);
  }

  submitApproval(organization: string, project: string, runId: string, input: ApprovalSignalInput, idempotencyKey: string): Promise<{ run?: ApiRun; signal?: string; target?: string }> {
    return this.request<{ run?: ApiRun; signal?: string; target?: string }>(`${this.scope(organization, project)}/runs/${encodeURIComponent(runId)}/approval`, {
      method: 'POST',
      headers: { 'Idempotency-Key': idempotencyKey },
      body: JSON.stringify(input),
    });
  }

  listEvents(organization: string, project: string, runId: string, after = 0): Promise<ApiEvent[]> {
    return this.request<ApiEvent[] | { events?: ApiEvent[] }>(`${this.scope(organization, project)}/runs/${encodeURIComponent(runId)}/events?after=${after}`).then((result) => Array.isArray(result) ? result : result.events ?? []);
  }

  private async collection<T>(organization: string, project: string, path: string): Promise<ApiCollection<T>> {
    return this.request<ApiCollection<T>>(`${this.scope(organization, project)}/${path}`);
  }

  listDefinitions(organization: string, project: string): Promise<ApiResource<Record<string, unknown>>[]> {
    return this.collection<ApiResource<Record<string, unknown>>>(organization, project, 'definitions').then((page) => page.items);
  }

  listRuns(organization: string, project: string): Promise<ApiRun[]> {
    return this.collection<ApiRun>(organization, project, 'runs').then((page) => page.items);
  }

  listApprovals(organization: string, project: string): Promise<ApiResource<Record<string, unknown>>[]> {
    return this.collection<ApiResource<Record<string, unknown>>>(organization, project, 'approvals').then((page) => page.items);
  }

  listRunners(organization: string, project: string): Promise<ApiResource<Record<string, unknown>>[]> {
    return this.collection<ApiResource<Record<string, unknown>>>(organization, project, 'runners').then((page) => page.items);
  }

  listEntitlements(organization: string, project: string): Promise<ApiResource<Record<string, unknown>>[]> {
    return this.collection<ApiResource<Record<string, unknown>>>(organization, project, 'entitlements').then((page) => page.items);
  }

  listModelRoutes(organization: string, project: string): Promise<ApiResource<Record<string, unknown>>[]> {
    return this.collection<ApiResource<Record<string, unknown>>>(organization, project, 'model-routes').then((page) => page.items);
  }

  getQuotas(organization: string, project: string): Promise<ApiQuotaData> {
    return this.request<ApiQuotaData>(`${this.scope(organization, project)}/quotas`);
  }

  getUsage(organization: string, project: string): Promise<ApiUsage> {
    return this.request<{ usage: ApiUsage }>(`${this.scope(organization, project)}/usage`).then((result) => result.usage);
  }

  listAudit(organization: string, project: string): Promise<ApiAuditEntry[]> {
    return this.collection<ApiAuditEntry>(organization, project, 'audit').then((page) => page.items);
  }

  listArtifacts(organization: string, project: string): Promise<ApiArtifactVersion[]> {
    return this.request<ApiArtifactVersion[]>(`${this.scope(organization, project)}/artifacts`);
  }

  getArtifact(organization: string, project: string, artifactId: string): Promise<ApiArtifactDetail> {
    return this.request<ApiArtifactDetail>(`${this.scope(organization, project)}/artifacts/${encodeURIComponent(artifactId)}`);
  }

  private async artifactContentResponse(organization: string, project: string, artifactId: string, versionId: string): Promise<Response> {
    const path = `${this.scope(organization, project)}/artifacts/${encodeURIComponent(artifactId)}/versions/${encodeURIComponent(versionId)}/content`;
    const response = await this.fetcher(joinUrl(this.baseUrl, path), {
      headers: await this.headers('*/*'),
      credentials: 'same-origin',
    });
    if (!response.ok) {
      const body = await response.json().catch(() => ({})) as ApiEnvelope<never>;
      throw new ApiError(body.error?.message ?? `Request failed with status ${response.status}`, response.status, body.error?.code, body.requestId);
    }
    return response;
  }

  async getArtifactContent(organization: string, project: string, artifactId: string, versionId: string): Promise<ApiArtifactContent> {
    const response = await this.artifactContentResponse(organization, project, artifactId, versionId);
    const body = await response.text();
    return {
      body,
      mediaType: response.headers.get('Content-Type')?.split(';')[0] ?? 'application/octet-stream',
      sizeBytes: Number(response.headers.get('Content-Length') ?? new TextEncoder().encode(body).byteLength),
    };
  }

  async downloadArtifact(organization: string, project: string, artifactId: string, versionId: string): Promise<Blob> {
    const response = await this.artifactContentResponse(organization, project, artifactId, versionId);
    return response.blob();
  }

  subscribeToEvents(organization: string, project: string, runId: string, listener: EventListener, onError?: (error: unknown) => void, after = 0): () => void {
    const controller = new AbortController();
    const url = joinUrl(this.baseUrl, `${this.scope(organization, project)}/runs/${encodeURIComponent(runId)}/events?stream=1&after=${after}`);

    void this.headers('text/event-stream').then((headers) => this.fetcher(url, { headers, signal: controller.signal, credentials: 'same-origin' })).then(async (response) => {
      if (!response.ok || !response.body) throw new ApiError('Event stream failed', response.status);
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = '';
      while (!controller.signal.aborted) {
        const chunk = await reader.read();
        if (chunk.done) break;
        buffer += decoder.decode(chunk.value, { stream: true });
        const frames = buffer.split('\n\n');
        buffer = frames.pop() ?? '';
        for (const frame of frames) {
          const data = frame.split('\n').filter((line) => line.startsWith('data:')).map((line) => line.slice(5).trim()).join('\n');
          if (data) listener(JSON.parse(data) as ApiEvent);
        }
      }
    }).catch((error: unknown) => {
      if (!controller.signal.aborted) onError?.(error);
    });

    return () => controller.abort();
  }
}

export function createApiClient(options: ApiClientOptions = {}): ApiClient {
  return new ApiClient({
    ...options,
    baseUrl: options.baseUrl ?? import.meta.env.VITE_AGW_API_URL ?? '/api/v1alpha1',
  });
}
