# Agents Gateway console redesign

## Outcome

Make the web console the operational UI for the same control-plane contract used by `agw`, with an OpenShift-inspired layout: project scope, resource navigation, list/detail views, command/action surfaces, and explicit empty/error states. The console must not present fabricated operational data or controls that have no working backend.

## Capability inventory

Backed by the current HTTP API and therefore exposed in the console:

- readiness and health checks
- project-scoped resource listing and resource detail for applied definitions
- apply/update of validated resources
- run creation, run detail, cancellation, approval, and reply signals
- live run events over SSE with reconnect
- artifacts, versions, safe previews, and downloads
- runner, model route, entitlement, approval, audit, usage, and quota collections

CLI-only/local capabilities remain documented as local workflows until the server exposes an equivalent contract: manifest validation/plan, manifest migration, signing-key generation, and local-token generation.

## Information architecture

- Project overview: readiness, active runs, pending approvals, recent failures, and quick actions.
- Workloads: Agents, Workflows, Runs, and Approvals.
- Resources: ToolSets, SkillSets, SandboxProfiles, ModelRoutes, Entitlements, Runners, and Artifacts.
- Governance: Audit and Quotas.
- Every resource has a list page, search/filter, empty/loading/error states, and a detail view showing the applied document, revision, digest, and available actions.
- Run detail is the primary troubleshooting surface: status, lifecycle actions, event stream, artifacts, and redacted payload details.

## Implementation sequence

1. Replace demo navigation/data with API-backed resource inventory and OpenShift-like project shell.
2. Add missing API client operations for generic resources and run signals.
3. Add resource list/detail views and real run actions; remove non-functional buttons and demo copy.
4. Keep artifacts as the existing safe renderer, embedded in resource/run detail.
5. Add unit coverage for navigation, API-backed empty/error states, resource detail, run cancellation, and approvals.
6. Build the console, run Vitest, deploy a preview revision through Coolify, and run browser E2E against the preview: connect, inspect project, list resources, open a run, cancel/approve where available, inspect events/artifacts, and verify no demo data is rendered.

## Acceptance criteria

- No hard-coded operational metrics or fake resources are shown in live mode.
- Every visible primary action maps to a tested API request or is removed.
- All supported resource kinds are reachable from navigation.
- Run cancellation, approval, and reply are usable from the run detail surface.
- The console remains usable at desktop and narrow viewport widths.
- Preview deployment passes health/readiness, build, unit tests, and browser E2E.
