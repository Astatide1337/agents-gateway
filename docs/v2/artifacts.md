# Claude-style artifacts

Agents Gateway v2 treats an artifact as a versioned, addressable result—not as
an instruction to execute. The implementation provides an immutable catalog,
authenticated content API, agent-authored artifact convention, and a console
workspace with preview, source, version selection, lineage, and download.

The product behavior is inspired by Claude-style artifacts: a response can
produce a self-contained document, code file, single-page HTML experience,
SVG, diagram, or interactive component. This project does not copy Anthropic
code or proprietary implementation details.

## Authoring from a sandbox

Codex may optionally place up to 16 strict descriptors in
`.agw/artifacts/*.json` and keep each referenced source file inside its
workspace:

```json
{
  "schema": "agents-gateway.artifact.v1",
  "title": "Latency explorer",
  "description": "Interactive view of request latency",
  "content_kind": "interactive_component",
  "media_type": "text/html",
  "source": "latency-explorer.html",
  "capabilities": ["sandboxed_scripts"]
}
```

Descriptors reject unknown fields, traversal, absolute paths, symlinks,
non-regular files, invalid UTF-8, incompatible content/media types, more than
16 artifacts, and more than 8 MiB of aggregate source. Linux `openat2` beneath
the workspace prevents path substitution and symlink escapes. A trivial text
status does not need a separate authored artifact.

After a successful model run, the adapter reads descriptors, uploads source
bytes through the private loopback broker, and emits one `artifact.created`
event per artifact. The generic immutable `output.json` remains mandatory and
is uploaded last, so authored artifacts cannot weaken the terminal run
contract. Sandboxes never receive object-store credentials or object keys.

## Durable contract and storage

`v2/pkg/artifactcatalog` defines the strict portable contract. An immutable
`Version` contains:

- schema, artifact ID, version ID, and UTC creation time;
- title, description, content kind, media type, renderer, preview metadata,
  and an allowlist security policy;
- content and source references with opaque catalog URIs, SHA-256 digests,
  sizes, and media types;
- optional parent/fork lineage.

The bytes commit to bounded local or S3-compatible object storage before the
validated version is inserted into the tenant-scoped PostgreSQL catalog. The
catalog stores trusted object keys server-side; browser and sandbox responses
contain only `artifact://catalog/...` references. Catalog reads are currently
bounded to 500 versions per response pending cursor pagination.

The local backend uses private regular files, no-follow directory traversal,
immutable no-overwrite commits, and SHA-256 validation. The content API
recomputes the complete digest into a private temporary file before sending
any response headers. A size or digest mismatch fails closed.

## Authenticated API

The control-plane API exposes:

```text
GET /api/v1alpha1/organizations/{org}/projects/{project}/artifacts
GET /api/v1alpha1/organizations/{org}/projects/{project}/artifacts/{artifactID}
GET /api/v1alpha1/organizations/{org}/projects/{project}/artifacts/{artifactID}/versions/{versionID}
GET /api/v1alpha1/organizations/{org}/projects/{project}/artifacts/{artifactID}/versions/{versionID}/content
```

Existing authentication, project authorization, RLS, and audit rules apply.
Tokens are sent only in authorization headers. Content is download-only at the
API boundary and includes `nosniff`, attachment disposition, same-origin
resource policy, a restrictive CSP, and private immutable caching. Object keys
and host paths are never returned.

## Console workspace

The React console provides:

- an artifact library and focused workspace;
- immutable version selection and lineage labels;
- Preview and Source tabs with keyboard and screen-reader semantics;
- escaped source, authenticated Blob downloads, and explicit loading, empty,
  error, and oversized-content states;
- safe previews for text/code, GitHub-flavored Markdown, HTML, SVG, diagrams,
  and interactive HTML.

Markdown is rendered as structured content rather than raw HTML. Embedded HTML
is dropped, links are displayed without navigation targets, and images are
replaced with inert alt text so previewing a document cannot make network
requests or introduce executable markup.

HTML, SVG, and component previews use a sandboxed `srcDoc` iframe without
same-origin privileges. Network, external calls, clipboard access, and scripts
are denied by CSP by default. Scripts run only for an
`interactive_component` version that explicitly declares
`sandboxed_scripts`; even then the iframe has no network or host credentials.

## Compatibility and boundaries

The contract already models immutable versions, parent edits, and forks, and
the console can display those histories. The first authored-artifact endpoint
creates version 1 of a new artifact. Mutation/remix APIs, public publishing,
share links, and collaboration are not yet implemented. Those are product
features—not prerequisites for secure local creation, preview, source, and
download—and must remain authenticated/private until explicitly designed.

Object storage and catalog writes cannot share one atomic transaction. A
catalog failure can therefore leave an unreachable object; a future bounded
garbage collector should remove orphaned objects after a retention window.

## Browser verification

The browser acceptance test starts the console on loopback and exercises the
artifact library in the latest `agent-browser` Chrome runtime. It covers
desktop and mobile layouts, immutable version switching, source and Markdown
views, a real download, iframe sandbox/CSP policy, WCAG A/AA violations, and
uncaught browser errors.

Install the harness once on a Linux development host, then run the test:

```sh
npx --yes agent-browser@latest install --with-deps
./v2/scripts/e2e-artifacts-browser.sh
```

On an AppArmor-constrained VM that blocks Chrome's unprivileged user namespace,
use `AGW_BROWSER_NO_SANDBOX=1`. The test still restricts navigation to the
trusted loopback Vite server; this option is for the UI test browser only and
does not alter Agents Gateway's sandbox isolation.
