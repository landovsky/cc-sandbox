[Index](README.md)

# Model docs: index

One picture of how sandboxed agent sessions are placed, fenced, granted access, connected and shown. Start with the unified model v2 (four docs). Open the area docs only for the detail behind a section.

**Open decisions:** the ❓ nodes for the build are in [the build plan §12](cc-unified-model-build-plan.md#12-open-decisions--nodes-under-cc-sandbox326q); older ones are in [the working log](cc-unified-model-log.md#decisions--nodes-updated).

## Reading order

1. The unified model, **v2.3** (current). Read these first, in order:
   1. [cc-unified-model.md](cc-unified-model.md): the threats, the security contract and its invariants.
   2. [cc-unified-model-tiers.md](cc-unified-model-tiers.md): what solo, friends and company each add, machines, third party.
   3. [cc-unified-model-cc-flow.md](cc-unified-model-cc-flow.md): CC Flow as a client (the portable cockpit).
   4. [cc-unified-model-log.md](cc-unified-model-log.md): changes per version, open items, decisions, conflicts, the old build list.
   5. [cc-unified-model-build-plan.md](cc-unified-model-build-plan.md): the whole build, phased, with owners, acceptance tests, the nightly prerequisites, the Accept list, risks, the review panel's findings and the ❓ list.
   - [unified-model.md](unified-model.md): **v1, superseded** by the four docs above. Kept for the history the area docs link to.
2. The five area docs (inputs to v1) (each one names its own ❓ nodes):
   - [model/mcp-gateway.md](model/mcp-gateway.md) (A): how agents reach MCP servers through one door with no MCP credential inside the fence.
   - [model/permissions.md](model/permissions.md) (B): who may do what: grant subjects, inheritance, consent, the Cedar policy decision API.
   - [model/cooperation.md](model/cooperation.md) (C): how sessions find and message each other, and where shared context and knowledge live.
   - [model/surfaces.md](model/surfaces.md) (D): how Tomas (and later client staff) sees and drives sessions: web, terminal, phone, push.
   - [model/orchestration.md](model/orchestration.md) (E): CC Flow as the orchestrator: placement, harness adapters, arming, managers, timing.
3. [unified-session-model.md](unified-session-model.md): v1. Where a session runs, how CC Flow drives it, the first sandboxed agent and its spin-up time. The unified model supersedes it where they conflict.
4. Inputs:
   - [cc-flow-on-kubernetes.md](cc-flow-on-kubernetes.md): proposal 68, CC Flow on Kubernetes (design only).
   - [coding-harness-control-surfaces.md](coding-harness-control-surfaces.md): what each coding harness lets an outside controller do.
5. Evidence (research behind the area docs, dated 2026-11-18, web-only):
   - [model/evidence/mcp-gateway-survey-1.md](model/evidence/mcp-gateway-survey-1.md), [model/evidence/mcp-gateway-survey-2.md](model/evidence/mcp-gateway-survey-2.md): MCP gateway products and specs (area A).
   - [model/evidence/permissions-survey.md](model/evidence/permissions-survey.md): authz engines and agent-grant patterns (B).
   - [model/evidence/cooperation-transport-survey.md](model/evidence/cooperation-transport-survey.md): inbox and message transports (C).
   - [model/evidence/cooperation-memory-survey.md](model/evidence/cooperation-memory-survey.md): agent context and memory tools (C).
   - [model/evidence/surfaces-web-terminals.md](model/evidence/surfaces-web-terminals.md): web attach to terminal sessions (D).
   - [model/evidence/surfaces-mux-dashboards-push.md](model/evidence/surfaces-mux-dashboards-push.md): multiplexers, dashboards, push (D).
   - [model/evidence/orchestration-survey.md](model/evidence/orchestration-survey.md): outside orchestration tools vs CC Flow (E).
