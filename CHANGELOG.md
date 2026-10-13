# Changelog

## Unreleased

**Security: any pod in the cluster could read, write and revoke any person's tool credentials at the
token service, and search any tenant's memory and knowledge.** The token service resolved, stored and
revoked grants and searched long-term memory and knowledge bases for whatever namespace, boundary and
user hash a request named. Its comment said only platform sidecars could reach it; nothing enforced
that. A user hash is not a secret (an agent holds the capability that carries it), so an agent's code
could take the credentials of whoever invoked it for any server and boundary, overwrite or revoke
anyone's grants, and read or poison another tenant's memory. Confirmed on a live cluster: a pod in
another namespace got HTTP 200 from `/v1/resolve`.

The token service now authenticates every request but the health probes with a projected
ServiceAccount token, reviewed by TokenReview (ADR 0154):

- `/v1/resolve` admits only an agent's egress sidecar (audience `ctxmesh-token-service`, mounted into
  the sidecar container alone). It resolves for the user and boundary of the run capability the sidecar
  forwards, in the sidecar's own namespace; a request naming another user, boundary or namespace, or a
  capability for another namespace, is refused with 403.
- `/v1/store` and `/v1/revoke` admit only the BFF.
- Long-term memory and knowledge search admit an agent's own container (audience
  `ctxmesh-token-service-agent`) for its own namespace and agent; per-user memory and a per-user
  knowledge base also need the run capability, and act only for its user. The BFF's knowledge test
  query may search any namespace.
- No token is 401, a bad or expired capability 401 with a code, a refusal 403, and an unreachable
  TokenReview 503 (a tool call then fails with 502, visibly).

The token service reads the capability public key from `bff-capability` and needs `create` on
`tokenreviews` (a new ClusterRole). A NetworkPolicy now admits only the BFF from the install namespace
and pods from other namespaces to its port (`tokenService.networkPolicy.enabled`, default on; it does
nothing on a CNI that does not enforce NetworkPolicy). Under it an agent in the install namespace cannot
reach the token service, and its tool calls time out. A knowledge-base reference to another namespace
no longer resolves. **Agents built on an older base image lose long-term memory and knowledge until
rebuilt**: their launcher sends no token. See [docs/upgrading.md](docs/upgrading.md).

The MCP grant key is now read from a Secret: `bff.mcp.grantHmacKeySecret` names one you create (key
`MCP_GRANT_HMAC_KEY`), or `bff.mcp.grantHmacKey` makes the chart create `ctxmesh-grant-hmac`. It was a
literal in the BFF's Deployment, readable by anyone who can read Deployments, and it shadowed a key
placed in `bff-adapters`. The run-worker now reads the same key; it had none, so a durable run's tool
calls hashed the user differently from the grant the BFF stored.

**Security: any pod in the cluster could invoke an agent in an AgentRegistry, past its registry's
isolation and its `allowedCallers`.** The launcher checked registry isolation and the callee's
`allowedCallers` and role rules only when a request carried an AMP envelope, the envelope was
whatever the caller wrote, and a request without one passed as ordinary traffic. Knative's
cluster-local route goes through Kourier, which every registry's network policy admits, so a pod in
another registry or another namespace reached any member by leaving the envelope off, or by writing
one. A chained hop also took its registry from the envelope the agent's own code echoed back,
`AGENT_REGISTRY_ID` and `AGENT_ROLE` could be set in `spec.env`, and the edge forwarded envelope
headers from outside. Confirmed on a live cluster.

A registry member now admits `/invoke` only from:

- an AMP call from another member's launcher, carrying the platform's attestation of the caller:
  the launcher trades its pod's token at the BFF for a five-minute attestation naming its agent,
  registry and role and the agent it is calling, and the callee requires the envelope to match it;
- its own async consumer;
- a request carrying a run capability minted for that agent, which is how the console, runs,
  workflows, sub-runs and the authenticating edge call it. Each of those mints a fresh capability
  per request, so the member admits one only while it is unbound and only the first time it sees
  it. The launcher binds every capability it admits to its own key before the agent's code gets
  it, so a bound one naming the agent is one its code handed on, for instance over an AMP hop,
  and a second presentation is a copy. Either is refused, under both postures; without that, any
  pod holding the copy could invoke the agent as the user for the capability's remaining life.

Anything else gets HTTP 403 `caller_not_allowed`. Agents in no registry are unchanged. A chained hop
claims its own registry. `AGENT_REGISTRY_ID` and `AGENT_ROLE` are now platform identity variables:
a value in `spec.env` is not used, and `PlatformEnvIgnored` names it. On a member the same holds for
`AMP_REQUIRE_ATTESTATION`, `MCP_CAPABILITY_PUBLIC_KEY`, `MCP_CAPABILITY_AUDIENCE` and
`BFF_POD_TOKEN_PATH`, so an author cannot switch one agent to accept unattested calls or verify
against a key of their own. The edge also removes `Ce-Ampattestation`, and the platform edge's
Gateway now removes `X-Ctxmesh-Run-Capability` and `X-Ctxmesh-Run-Proof` from every request before
ext-auth runs: ext-auth replaced a caller's capability header only when it minted one, so on any
allow without a capability, or with ext-auth off, the caller's own reached the agent. This adds a
`ClientTrafficPolicy` to the edge chart and needs Envoy Gateway v1.2 or later. The BFF's
attestation edge remembers a rejected pod token for a minute, so repeating one does not cost a
TokenReview each time. The edge removes
`X-AMP-Envelope`, `X-A2A-Envelope` and `X-AMP-Attestation` from inbound requests. An agent whose
launcher cannot get an attestation fails the hop with HTTP 503 `attestation_unavailable` instead of
sending it unattested. Every agent's launcher, member or not, refuses with HTTP 401 a capability
that comes back to it: one bound to its own key, or one it has already bound. A run's
output-schema re-ask now carries a capability of its own instead of resending the first.

**Behaviour change:** a request to a member's `/invoke` that does not come through the platform,
such as a `curl` to its cluster-local URL or through an edge with ext-auth off, is now refused. Go
through the console or `POST /api/invoke` and `/api/runs`. Every registry member rolls one new
revision on upgrade, with its BFF token, the capability key and the posture added; an approved
revision stays approved. `controllerManager.amp.requireAttestation: false` keeps admitting
unattested calls during a staged upgrade, while still refusing a bad attestation; the
`amp.admission` span attribute says how each call was admitted, so the flag can be turned back on
once `unattested` stops appearing.

Async deliveries are held to the same rule. A member's launcher took any POST with a `Ce-Id` header
as a CloudEvent and admitted its envelope as its own async consumer, so any pod could reach a member
that way too. A delivery is now admitted only with the platform's attestation of its sender, made
for the receiver and carried as the CloudEvent extension `ampattestation`, and the envelope must
match it; anything else is HTTP 403 `caller_not_allowed`, before the message id is recorded. The
JetStream dispatcher attests the sender of every hop it delivers: the publish edge records which
agent's run published it, and the dispatcher reads that agent's registry and role when it delivers,
not delivering a hop whose sender has left the registry. A producer publishing straight to a
registry's Knative Broker must attach its own attestation, fetched from
`POST /api/internal/amp/attestation` with the sending agent's pod token.

**Changed: an agent's launcher serves its tools; the tool-discovery sidecar is gone.** A tool-bound
agent's pod has one container fewer. The agent container mounts its tool manifest at
`/etc/agent/tools/tools.json` (named by `TOOLS_JSON_PATH`) and the launcher serves it on
`localhost:2999` (`GET /tools`, `GET /events`) as the sidecar did. A binding change reaches a running
agent when the kubelet projects the updated ConfigMap, usually within a minute, instead of through an
HTTP push from the controller to every pod; the controller no longer reads pods. This is the same path
the egress sidecar's route table already took, so a newly bound remote tool now appears in the
manifest when its route does, not up to a minute before it. An agent image built on an older launcher
still finds its tools: the Python and TypeScript SDKs read `TOOLS_JSON_PATH` when `:2999` does not
answer. Removed: the `agent-discovery` image, the controller's `DISCOVERY_IMAGE` and the chart value
`controllerManager.injectedImages.discovery`. `ctxmesh dev` runs no discovery container either.

**Changed: the egress sidecar resolves tool credentials only through the token service.** Its
embedded Kubernetes backend, used only when `TOKEN_SERVICE_URL` was unset, which no install path
does and which could not have authenticated if one did, is removed, and the sidecar image carries no
Kubernetes client. The sidecar refuses to start without `TOKEN_SERVICE_URL`, and the controller
refuses to deploy a tool-bound agent on an install without one, naming the setting, rather than
deploy a pod whose sidecar crash-loops. The controller no longer reads `MCP_CREDENTIAL_NAMESPACE`.

**Fixed: a namespace enforcing the restricted Pod Security Standard refused every tool-bound agent.**
The egress sidecar had no securityContext, and a pod is judged by its worst container. It now runs
non-root with no privilege escalation, every capability dropped, the RuntimeDefault seccomp profile
and a read-only root filesystem. A sidecar-mode tool container is still the binding author's image
and carries none, so an agent with one is still refused there.

**Changed: every sidecar the platform injects has CPU and memory limits.** The egress sidecar is
limited to 500m and 256Mi, with `GOMEMLIMIT` below that, sized from a load measurement of the
sidecar; the trace collector gains a 250m CPU limit beside its 256Mi. Every agent takes one new
revision on upgrade so the limits reach running pods.

**Changed: `scaling.concurrency` above 1 requires `resources.cpu` and `resources.memory`.** One
replica then runs several agent runs at once, and the scheduler places it only by what it declares,
so the API server refuses such a spec without both. `scaling.concurrency` is new in this release, so
no existing object is affected.

**Changed: the controller caches its own objects, not the cluster's.** It held every ConfigMap,
Service, ResourceQuota, Deployment, Job, CronJob, NetworkPolicy, Knative Service, Trigger, Broker and
ScaledObject in the cluster, every Knative Revision, and the full metadata of every Secret, so its memory
grew with everything else running beside it. It now labels what it creates of those kinds
`agents.ctxmesh.ai/managed-by=ctxmesh-controller` and caches them only where they carry the label, plus
the install namespace's ConfigMaps and Deployments and Knative's `config-features`. Revisions are read
when needed. ServiceAccounts stay cached in full. The Secret watch keeps only each Secret's name,
namespace, labels and resourceVersion; a Secret applied with `kubectl` had carried its data into the
controller's memory in its `last-applied-configuration` annotation. Objects an earlier controller created
get the label the first time it reads them after the upgrade.

The label is reserved: do not remove it or set it yourself. Without it the controller stops seeing changes
to its own object until it next reads it and puts the label back; on an object of yours it makes the
controller cache it. Knative copies a Knative Service's labels onto the Configuration and Route it derives
from it, and the Route's onto the Kubernetes Services it creates, so those carry the label too. See
[Upgrading](docs/upgrading.md).

**Fixed: a queue-depth `AgentScalingPolicy` never scaled anything.** Its ScaledObject used KEDA's
generic `metrics-api` scaler, pointed at `http://<agent>-eventing.<namespace>.svc.cluster.local/metrics`,
which nothing serves, so KEDA held the agent at its minimum. On an install whose async backend is NATS
JetStream (`bff.asyncBackend: jetstream`), the policy now emits KEDA's `nats-jetstream` scaler, which
reads the hops waiting for the agent: about one replica for every 10 waiting, and one waiting hop wakes
an agent scaled to zero. For that count to mean one agent, every registry member now has a durable
consumer of its own, `agent_<namespace>_<agent>`, and the BFF publishes a hop on
`ctxmesh.a2a.<registry>.<namespace>.<agent>`. Hops published before the upgrade are still delivered.
A hop to an agent in no registry now waits in the stream until the agent joins one, or until the
stream's retention drops it, and the BFF logs it when it is published; before, it was retried for about
six minutes and dropped. Without the JetStream backend, including when async hops ride Knative
Eventing, there is no queue to read: the policy creates no ScaledObject, deletes one it created before, reports
`Ready=False` with reason `QueueDepthNeedsJetStream`, and the agent runs a fixed replica count kept
between the policy's `min` (at least 1) and `max`. A queue-depth policy for a serving or job agent
reports `QueueDepthNeedsEventing` and creates none. `spec.queueRef` is no longer used. A policy whose
trigger changes from queue-depth, or whose agent is deleted, now deletes its ScaledObject. The
controller reads `ASYNC_BACKEND` and `NATS_URL`, which the chart sets from `bff.asyncBackend` and
`bff.natsURL`; KEDA reads the NATS monitoring port, 8222, on the `bff.natsURL` host, and the bundled
NATS now admits the KEDA operator (namespace `keda`, label `app=keda-operator`) to that port only.
Nothing changes on an install without these values.

**Fixed: the controller reset an eventing agent's replicas on every reconcile.** It wrote
`spec.replicas` on the agent's Deployment each time, so any scale-up by KEDA was undone at the next
reconcile, which the scale-up itself triggered. While a queue-depth ScaledObject is in effect the
controller now sets the count only when it creates the Deployment. The `executionModel` field
description, which said an eventing agent is a Knative Service with a Trigger, now says what the
controller renders: a Deployment and an `<agent>-eventing` Service, a Trigger on the registry's broker
when Knative Eventing is installed, and deliveries from the agent's JetStream queue on that backend.

**Changed: before its first agent, a workspace's sidebar shows Home, Agents and Providers.** With no
agent the other pages have nothing to show: no runs, no cost, nothing waiting for approval, nothing
to stop. The sidebar now lists only the destinations Home's first-run checklist walks through, says
the rest appears once there is an agent, and widens on the first page after one is created. It asks
what Home asks, the agent list for the selected workspace with drafts included, and keeps the whole
nav when that list is refused or fails. Every page still opens from a link, its URL, or the command
palette, which keeps listing them all.

**Fixed: the Approvals and Alerts nav entries showed to people who could not open them.** Their
gates asked about `list workflows` and `list alertpolicies`, which `/api/capabilities` never probed,
and the console reads a permission nobody asked about as allowed. Both are now probed, for `list`
only, so `/api/capabilities` reports `workflows` and `alertpolicies`; the Approvals count is no
longer polled for someone who cannot see the queue. A console test now fails when a nav gate names a
permission the BFF does not probe, checked against a census the BFF's own test writes from the probe
it runs.

**Removed: the console's design gallery.** `ui/src/design` held the M13 wireframes, served at
`/design` only by the dev server or a `VITE_DESIGN_GALLERY=1` build. Nothing the console ships used
them, and every screen they drew has since been built. `VITE_DESIGN_GALLERY` no longer does
anything, and `make ui-build` no longer runs `hack/ui-no-internal-routes.sh`, which only checked that
the gallery stayed out of a production build.

**Removed: the console's `/soon/…` placeholder pages.** A nav destination that a later milestone
would ship used to lead to `/soon/<id>`, a page naming the milestone it would arrive in. Every
destination has since shipped and nothing linked there any more; an old `/soon/…` link now shows the
console's not-found page.

**Changed: model routes, secret bindings and agent registries open on one detail page.** The three
pages were copies of one another that had drifted: an agent registry loaded with a bare "Loading…"
line, answered a missing registry with a different empty state, and had no Retry when its read
failed. They now share one page: the same header with its status and Edit and Delete, the record in
a rail beside what the resource does, the same loading, not-found, permission and retry states, the
edit drawer at `?edit=1` and the typed-name delete at `?delete=1`. Every field and action each page
showed is still there, and Edit and Delete are still asked of the item's own namespace.

**Changed: the tool catalog is a tab of MCP servers.** The catalog lists the tools those servers
expose, and it was a separate nav entry beside them. It is now the Tools tab of the MCP servers page
(`/tools/mcp-servers?tab=tools`), under the same header, and a server's drawer opens it. Binding a
tool to an agent works as before. `/tools/catalog` redirects there, keeping its query, so bookmarks
and the MCP sign-in's return still land on the catalog. The Tool catalog nav entry is removed.

**Changed: MCP approvals is a tab of Approvals.** The console had two approval queues in the nav:
runs waiting on a person, and MCP servers waiting on an operator. Approvals now holds both, with
runs as the first tab and MCP servers as the second (`/approvals?tab=mcp`), which shows to a caller
who may update agent registries, as its nav entry did. Each queue keeps its count, refresh, poll,
drawer and decisions. `/tools/approvals` redirects to the tab, and the links that pointed at it
(an MCP server awaiting approval, a requirement on the create-agent review, a server queued after
it is added) go there. The MCP approvals nav entry is removed.

**Changed: one way to author an agent. The config builder is New agent's agent.yaml entrance.** The
console had two: the New agent wizard, and the config builder, a second form over the same fields
with its own preview and apply. New agent now has a fourth way in, Write the agent.yaml
(`/agents/new?mode=yaml`): the file the CLI expands, typed or pasted, for any field the form does not
carry. It needs no connected provider, and the no-provider prompt offers it. Like the other ways in it
ends at the shared review, where Preview shows the expanded CRDs and Create applies them into the
chosen workspace. The three fields only the config builder had, the budget's soft alert threshold and
the eval gate's mode and scorers, are now in Configure's Optional step; before, enabling the eval gate
there failed on a scorer the step did not show. `/config` redirects to the new entrance. The Config
builder nav entry is removed.

**Security: a registry in one namespace could share another namespace's team scratchpad and
credential boundary by copying its `registryId`.** A registry's id is chosen by its author and was
the whole key for what its members share: the shared scratchpad (`sessionMemory.scope: shared`)
lived at `mem:shared:<registryId>:`, and the trust boundary a person's grants are scoped to was
`r:<registryId>`. A developer who created a registry in their own namespace with another team's id
had their agents read and write that team's shared scratchpad, and their agents' egress sidecars
accepted run capabilities minted for that team's agents. Both are now qualified by the registry's
namespace (`mem:shared:<namespace>/<registryId>:` and `r:<namespace>/<registryId>`), so registries
in different namespaces are different registries whatever their ids. Stored grants were already
keyed by the namespace consent ran in, so no grant was resolved across namespaces.

A `registryId` must now also be unique within its namespace, because pods, network policies and the
boundary are keyed by it: a registry declaring an id that an older registry in the namespace already
holds is `Ready=False` with reason `DuplicateRegistryId`, naming the holder, and has no members until
the holder is deleted. `MultiRegistryConflict`, which the API documented and nothing set, is gone
from the documented reasons.

**Behaviour change:** personal grants scoped to a registry were stored under the old boundary and
are not found under the new one, so each person reconnects once per registry (the run asks for
consent as it would for a new grant). Shared scratchpad contents written before the upgrade are not
carried over. Registry members bound to tools roll one new revision, because their egress sidecar's
boundary changes. See docs/upgrading.md.

**Fixed: a clone of a release installed images that were never published.** The chart in the
tree said `v0.1.0-beta.1` at every release, because the release stamped only the package it
published. A clone of any tag therefore installed the first beta's control plane against that
tag's CRDs and templates, and three images the controller injects into agent pods,
`agent-otel-collector`, `agent-discovery` and `egress-sidecar`, that were first published with
beta.7; the collector goes into every agent pod, so no agent started. `Chart.yaml` now carries the
product version, written by `hack/bump-version.sh` with the other pinned versions and checked in CI.
The release refuses a tag it disagrees with and, before it publishes the Release page, pulls every
first-party image a clone of the tag renders; the nightly does the same on `main`. A milestone
branch names its upcoming release, so installing one from a clone needs images built from it.

**Fixed: the README's production command did not render.** The production profile requires
`bff.mcp.grantHmacKey` and `bff.mcp.credentialNamespace`, and the command set neither, so
`helm install` stopped at its first guard. It also never said to create the `token-service-tls`
Secret, without which the token service refuses to start under that profile. The command now sets
both and creates the Secret first. CI renders the README's own command (`hack/install-truth.sh`,
which also fails if the block stops creating the Secret), and the image checks list that command's
images from the README too, where they had used a copy that carried the two missing values.

**Changed: an `/api` route the server cannot serve answers 501, not 404.** Each route is now
registered once. When something it needs is missing (the caller-scoped cluster client, as under
`ctxmesh dev --ui`; the trace store; the agent invoker; the config expander), it answers `501`
naming what is missing, for example `caller-scoped cluster access is not configured on this
server`. Some routes had no such answer and fell through to a 404, among them
`/api/agents/census`, `/api/kills`, `/api/audit`, `/api/runs/{id}/tree` and
`/api/runs/{id}/fixture`, so the console could not tell a missing feature from a missing object.

**Removed: `GET /api/metrics/…`.** It answered 501 on an install without Prometheus and 404 on one
with it; nothing ever served it. Cost is `GET /api/cost`, and `GET /api/metrics/eval-gated` is
unchanged. `GET /api/traces/{id}/…` paths other than `/detail` now answer 404 rather than 501.

**Changed: an agent's registry is recorded in `status.registryRef`.** The controller writes the
name of the AgentRegistry an agent belongs to, the one its current revision was built with, into
`AgentDeployment.status.registryRef`, empty when it belongs to none. The BFF reads it to choose the
trust boundary a personal grant is scoped to, instead of listing the namespace's registries and
matching their selectors itself, so the two can no longer disagree about membership. Until the
controller has reconciled an agent once after an upgrade, which it does for every agent when it
starts, that agent's runs use its own boundary. An AgentRegistry's `status.members`, an AgentTeam's
roster check, a Workflow's step check and an inline workflow run's check read it too: an agent two
registries select is a member of the one it joined, and a team or workflow naming the other is not
Ready, with a message naming the registry the agent belongs to. Before, each of those matched the
selectors itself and counted the agent in both.

**Removed: `GET /api/end-user/runs`.** The end-user "my runs" list at an agent's own hostname had
no caller: the chat page never offered it, and no SDK, script or page of the docs named it. It goes
with the run-store query behind it. A run an end-user owns is still readable at
`GET /api/runs/{id}`.

**Fixed: an install outside the `ctxmesh` namespace gave its agents addresses that did not exist.**
The controller wrote the `ctxmesh` namespace into every platform address it gave an agent
(`MODEL_GATEWAY_URL`, `GATEWAY_UPSTREAM_URL`, `BFF_INTERNAL_URL`, `ASYNC_PUBLISH_URL`, the state
layer in `MEMORY_BACKEND_ADDR` and `TENANT_QUOTA_ADDR`, and `OBJECT_STORE_ADDR`), into the egress
rule of every registry and tenant network policy, into the namespace a Tenant may never claim, and
into where it wrote the model gateway's config and provider Secret mirrors. The chart lets
`namespace` change, so on any other install every model call, memory read and capability bind went
to a Service that was not there, and the gateway was never configured. All of them now come from the
controller's own namespace, which it reads through the downward API: the chart installs every
platform service there. The chart generator refuses a platform address that names the default
namespace, and a test fails on one in the controller. Agents of a default install are unchanged;
agents of any other install roll one new revision, and an approval carries to it.

**Fixed: a `spec.env` name the platform also sets put the variable in the pod twice.** Beyond the
identity variables, the controller sets 48 variables on the agent container, and for most of them
(`MODEL_GATEWAY_URL`, `AGENT_PORT`, the `BUDGET_*` and `TENANT_*` caps, `MEMORY_PORT`,
`MEMORY_KEY_NAMESPACE`, `STATELAYER_PROXY_URL`, `KNOWLEDGE_BASES` and others) a `spec.env` entry of
the same name left the container carrying both. Kubernetes uses the last entry of a repeated name,
so which one won depended on where the controller happened to add the variable: an author could
point `MODEL_GATEWAY_URL` past the in-pod proxy that enforces a budget, a tenant's caps and
guardrails, while the platform silently beat the author's value for the tenant caps and memory
wiring. Now one rule decides every platform variable. When the platform sets a variable for an
agent, its value is used, the `spec.env` entry is dropped, and the agent's `PlatformEnvIgnored`
condition names it; the field that sets the variable (`port`, `budget`, `guardrailPolicyRef`,
`allowedCallers`, `sessionMemory` and so on) is how to change it. A name the platform does not set
for that agent is used as given, such as `PROMPT_FILE` without a `promptRef`, or `TOKEN_SERVICE_URL`
on an install with no token service. The identity variables, and a member's attestation wiring, stay
reserved even where the platform leaves them unset. No variable is overridable any more, including
`BFF_INTERNAL_URL` and `AGENT_ALLOWED_CALLERS`, which were. The condition replaces
`IdentityEnvIgnored`, which the controller removes. An agent whose `spec.env` named a platform
variable rolls one new revision with the platform's value; an approval carries to it.

**Fixed: per-user memory, per-user knowledge bases and per-user quota refused every run under the
default install.** The launcher checked run capabilities against `MCP_CAPABILITY_AUDIENCE`, which
the chart leaves unset, so it expected an empty audience while the BFF mints for
`ctxmesh-credential-plane`. Every launcher check now defaults to the BFF's audience, through one
constructor.

**Fixed: async AMP hops to eventing and job agents were lost, and those agents could not call other
agents.** Only serving agents were recorded in the capability registry, so the JetStream dispatcher
acknowledged every hop to an eventing or job agent as outside its sender's registry and dropped it,
and the BFF refused to attest either kind as an AMP caller (HTTP 403) or to accept its async
publishes. The dispatcher also delivered to `<agent>.<namespace>.svc`, and an eventing agent has no
Service by that name. Eventing and job agents in a registry now have a row with their registry and
role, and every row records where the agent receives deliveries: its Knative route when serving,
its `<agent>-eventing` Service when eventing, and nowhere for a job, which can make AMP calls but has
no Service to receive one. A hop to an agent with no row or no address is now retried instead of
acknowledged, and the dispatcher logs every failed delivery with its reason, so a hop to a job agent
fails where it can be seen once its retries are spent. Migration 0031 adds the address; a hop to an
agent whose row the controller has not rewritten since the upgrade is retried until it has.

**Fixed: an async AMP message whose delivery failed was lost instead of retried.** With session
memory on, an agent's async consumer recorded a message id as seen before invoking the agent and
never cleared it, so when the invoke failed the broker's retry was answered 204 as a duplicate and
the message was dropped without reaching the dead-letter queue. The consumer now claims the id while
the agent processes the message, records it as done only once the agent succeeded, and releases it
when the agent failed, so a retry is processed and a message that keeps failing reaches the
dead-letter queue. A duplicate that arrives while the first delivery is still being processed is
answered 503 and retried. A claim expires after two minutes, so a message whose consumer died while
invoking the agent is processed by a later JetStream retry; Knative Eventing's retries finish sooner,
so there it goes to the dead-letter queue. The state-layer proxy's `/dedup` endpoint takes an `op`
(`claim`, `complete`, `release`); a launcher that sends none gets the previous behaviour.

**Fixed: synchronous AMP calls, spawns, handoffs and suspended delegations to an eventing agent
failed.** Each was addressed to the agent's Knative route, `<agent>.<namespace>.svc`, and an
eventing agent serves only at `<agent>-eventing`. A launcher now gets the receiver's address with
its attestation, when the receiver is in its registry and the platform records one, and the BFF
runs a spawned, handed-off or delegated run at the address the capability registry records for the
target. Agents with no recorded address keep their Knative route.

**Security: a handoff or a suspended delegation could send a run to an address the agent chose.**
The BFF stored the `targetEndpoint` of a handoff, and the `endpoint` of each delegation in a
suspend marker the agent's own code writes, and the run-worker then sent the new run's input and
run capability there. Both are now ignored, as a spawn's already was: the platform derives the
address. A handoff no longer needs `targetEndpoint`.

**Security: the Langfuse key was readable in every agent's pod spec.** The trace collector carried
it as a literal environment value, so anyone who could `kubectl get pods` in an agent namespace
could read it. And an agent whose namespace had no `langfuse-otlp` Secret was given the *platform*
namespace's key. Every Langfuse credential is now a Secret reference: on the collector, the
gateway and the feedback hook. Only the agent's own namespace counts.

**If you configured a `langfuse-otlp` Secret, rotate that Langfuse key.** Knative keeps old
revisions, and those still hold the key as a literal.

**Security: a model route could resolve to a Secret outside its namespace.** The gateway copied
each provider Secret into its own namespace under the Secret's bare name. Anyone allowed to create
ModelRoutes and SecretBindings, which includes the `developer` role, could name a Secret after a
platform Secret, set the route's `apiBase` to a host they control, and receive the platform
Secret's value as the API key. Two namespaces with a binding of the same name also shared one
credential. Copies and their environment variables are now named per namespace. A route whose
copy cannot be written is not served. A route with both `apiBase` and a binding is checked like
any other bound route. Every bound route's gateway variable is renamed, so the gateway rolls once.

**Behaviour change:** an agent exports traces only if its own namespace holds a complete
`langfuse-otlp` Secret, with keys `otlp-endpoint`, `public-key` and `secret-key`. The new
`TraceExport` condition on each AgentDeployment says whether it is exporting and, if not, why.
Every agent rolls one new revision on upgrade. A revision approved before the upgrade stays
approved under its new name.

**Security: a copied run capability can no longer be spent.** The platform's proof-of-possession
design had never taken effect: nothing bound capabilities, and the BFF accepted bearer tokens. Now
each agent's launcher binds the capability to a key that exists only in its memory, before the
agent's code receives it. It then proves possession on every call to spawn, handoff, discover,
async publish and guardrail events. Every agent now gets `BFF_INTERNAL_URL`, and a Tenant's network
policy allows it.

**Security: registering an MCP server or connecting a provider contacted the URL before checking
the caller.** Both flows probe the URL in the request body, and both probed first. Anyone holding
any bearer string could make the BFF send requests to an address of their choosing and learn from
the error whether it answered. Both now check the caller's permission first: a refused caller gets
403 (401 for a token the API server rejects) and nothing is contacted. Registration without the
control-plane store is refused with 501 instead of failing after the probe.

**Security: connecting to an OAuth MCP server could send the authorization code to any address.**
The request that starts a user's OAuth consent could name its own token endpoint, and the BFF
then sent the authorization code and PKCE verifier there. Anyone who could see the server could
use it. A server whose OAuth endpoints were saved when it was registered now keeps them: a request
naming a different `authorizationEndpoint` or `tokenEndpoint` is refused with HTTP 400. Servers
registered before those endpoints were saved can still be given them.

**Security: anyone who could see an older OAuth MCP server could still choose its token endpoint.**
For a server registered before its OAuth endpoints were saved, the request that starts consent
could name them, and the BFF sent requests there, the authorization code and PKCE verifier
included. A viewer could use it to reach internal addresses. Naming those endpoints now takes
permission to update the server, the permission saving them takes. Anyone else gets the endpoints
discovered from the server's own URL.

**Security: an OAuth consent link could connect someone else's account to yours.** The callback
that finishes an MCP OAuth connection trusted its `state` alone. Anyone who could start a consent
could send the authorization link to someone else, and if they approved it, their upstream tokens
were stored under the sender's identity. The response that starts a connection now sets a cookie
in that browser, and the callback refuses any other browser before the code is exchanged. The
cookie is `__Host-` prefixed over HTTPS. Registering an OAuth server now always uses the BFF's own
callback as its `redirect_uri`; a `redirectUri` in the request is ignored. **Behaviour change:** a
connection must be finished in the browser, and at the address, where it started. With
`bff.consoleURL` set, start consent from the console at that URL: Connect from the console
reached at another address is refused. Connect in an agent's own chat goes through the console
(below).

**Fixed: Connect in an agent's own chat failed with `bff.consoleURL` set.** The connection is
bound to the browser by a cookie on the console's address, which a chat at the agent's own
hostname cannot set. Connect now gets a one-minute, single-use ticket from the agent's address,
opens the console's new `/connect` page in a pop-up, and hands the ticket to that page by
message; the page shows whose account it is connecting, for which server and agent, and begins
the connection on the console, in the same browser, when you press Continue. The console accepts
a ticket only from the hostname of the agent it was issued for, and never from a link. Pop-ups
must be allowed for the agent's address. This works with the edge's `baseDomain: localhost` too:
a base domain that can never be delegated (`localhost`, `internal`, `test`, `invalid`, `example`,
`local`) is where the agents live, not a public suffix. Any other single label is treated as a
public suffix, so a console at `console.corp` has its agents under `console.corp`.

**Security: the dataset API checked only that a token was valid.** Any authenticated caller could
list and read the datasets of any namespace, label or pin them, and start an export that copied any
agent's traces into a dataset of their choosing. A label named only its case, so a writer could
label a case in another namespace. Every dataset request is now authorized as the caller on
`evalsuites` in the dataset's namespace, before anything is read: reading needs `list`, and
exporting, labeling, adding a run and pinning need `update`. An export also needs read access to
the agent whose traces it copies. A label's case must belong to the dataset in the path. Refusals
are 403, or 401 for a token the API server rejects. An end user's token is refused with a 404
before any call to the API server. A namespace that is not a valid name is a 400, as is an export
naming an agent that is not one. Reading or pinning a dataset that does not exist is a 404 and no
longer creates it. The shipped viewer role can read datasets in the namespaces it is bound in, and
the developer and operator roles can also change them.

An export run belongs to whoever started it, so they can follow and cancel it; before, its creator
got a 404 and anyone who could read an agent named like the dataset could read or cancel it. Others
need permission to change that namespace's datasets. An export copies a trace that a run maps only
if its creator could open it on the trace page: they made that run, or the run belongs to the agent
being exported. A trace shared through a workflow or a handoff belongs to the run that started it,
so it is withheld, and the export's result counts what it withheld.

**Fixed: a passing API-server error signed the person out.** Starting a knowledge-base ingestion
or a dataset export reads the caller's identity, and any failure to read it answered HTTP 401, which
the console takes to mean the session has expired. Only a token the API server rejects is a 401 now;
any other failure is a 500 and is logged.

**Security: reading a knowledge base was enough to change it.** Uploading a document to a
KnowledgeBase and starting its ingestion checked only that the caller could read it, so the shipped
`viewer` role could add documents to a corpus other people's agents retrieve from, and start
ingestions that spend the embedder. Both now need `update` on that KnowledgeBase (403 otherwise),
checked before it is read; the `developer` and `operator` roles have it. The console no longer offers Upload or Ingest to a caller without it, and
says why.

**Security: anyone who could read an agent named like a knowledge base could read or cancel its
ingestion.** An ingestion run was created without a creator, so the person who started one got a
404 reading it, and access to the run was decided by an AgentDeployment of the knowledge base's
name. The run now records whoever started it, who reads and cancels it by ownership; anyone else,
and any scheduled re-ingest, needs `update` on that KnowledgeBase. Nor do an ingestion's runs, or a dataset
export's, appear any longer in the Runs list of an agent named like the knowledge base or the
dataset.

**Fixed: the replay stepper showed every step of a recorded run as a gap.** `GET
/api/runs/{id}/fixture` stopped reading the run's events before the Postgres run store had
delivered any, so on the production store a recorded run had no steps to join its recording to.
And the trace page asks by trace id, while the step read and the fixture lookup used that id as a
run id, so from a trace the stepper found neither the steps nor the recording. Both now read the
finished run by its run id, until its events are delivered. A run still in progress shows its
recording with no step timeline until it ends. A finished run's events that cannot be read from
Postgres are now an error, where the stream used to end early as if complete. A trace that several
runs share now always resolves to the same run, the earliest root, on both run stores; before, the
Postgres store returned whichever row it found first and the in-memory store a random one.

**Fixed: the chat streamed raw event JSON and never showed an approval or consent prompt
mid-run.** The run event stream (`GET /api/runs/{id}/events`) put each event in its `data:` line as
a JSON envelope, while the console and the SDKs read that line as the payload itself. The chat
showed tokens and step labels as JSON until the run ended, and a run that stopped for an approval
or a consent left the chat on an open stream instead of showing the prompt. The Python and
TypeScript SDKs' `RunsClient.stream` yielded the envelope instead of the text. Each frame now
carries the payload itself (a token's text, a state's new status, a step's metadata), one `data:`
line per line of it, with the sequence number in `id:`. A client that parsed the envelope reads the
payload directly instead. After a consent or an approval, the chat resumes the run's stream after
the event that paused it.

**Fixed: no shipped role could watch an agent's live output.** The live output on an agent's page
reads the agent's pod log as the caller, and no shipped role granted it, so the console hid it from
everyone. The `developer` and `operator` roles now grant `get` and `list` on `pods` and `get` on
`pods/log`, in the namespaces they are bound in, as Kubernetes' own `view` and `edit` roles do. The
`viewer` role does not.

**Fixed: no shipped role could use skills.** `/api/skills` authorizes every call against the
`skills` resource, and no shipped role granted it, so it answered 403 to every persona. The
`viewer` role now grants `get`, `list` and `watch` on `skills`, and the `developer` and `operator`
roles grant the same verbs they hold on `promptversions`, in the namespaces they are bound in.

**Fixed: an agent's author could put its traces and spend under another agent's name.**
`POD_NAMESPACE`, `AGENT_NAME`, `AGENT_VERSION` and `TENANT_ID` in `spec.env` replaced the platform's
values, and the launcher tags every trace `agent:<POD_NAMESPACE>/<AGENT_NAME>` and
`version:<AGENT_VERSION>`. An author could therefore plant traces and cost rows under another
namespace's agent (a trace that no run maps is authorized by that tag), score one version as another,
which steers regression detection, rollback and canary decisions, and charge calls to another
tenant's quota and spend ledger. These four are now always the platform's. A value for one in
`spec.env` is not used, and the agent's `PlatformEnvIgnored` condition names it until it is removed.
An agent that set one rolls one new revision with the platform's values; an approval carries to it,
and the old revision is not kept as a canary arm. Setting `TENANT_ID` in `spec.env` of an agent whose
namespace a Tenant owns no longer produces two `TENANT_ID` entries, which the Knative Service refused.

**Fixed: a person who could list tenants but read cost in one namespace never saw its cost.** The
shipped `viewer` role bound cluster-wide grants listing tenants, so with `operator` bound in
`team-a` the breakdown demanded a tenant, the Cost page picked the first one, and the tenant's
breakdown refused them, because naming a tenant needs cluster-wide cost access. A tenant is now
required only of a caller who may read cost cluster-wide; anyone else is answered for their
workspaces, whatever tenants they can list. That refusal now carries the code `tenant_required`,
and the Cost page asks for no tenant first and picks one only when told to.

**Fixed: a tenant's or a workspace's cost breakdown could show a false zero.** The breakdown cut
its page from every agent in the cluster and only then kept the ones the caller may see, so an
agent that ranked below the first page came back as an empty page, a $0 total and a next cursor,
and the Cost page said "No cost data yet". Paging with `limit=1` also counted every agent in the
cluster. The breakdown now keeps the tenant's or the caller's namespaces first: pages, `nextCursor`
and the total cover only those agents, and the total is the sum of all of them, not of one page.
The Cost page no longer shows the empty state on a page that has another page beside it.

**Fixed: an agent's live output never showed.** The log tail on an agent's page named no
container, and Kubernetes refuses a log read without one on a pod with more than one container,
which every agent pod is (Knative runs `queue-proxy` beside it). The tail now reads the agent's own
container, or the pod's `kubectl.kubernetes.io/default-container`, unless the request names one.
It also showed an error while a new agent was starting: a pod that is scheduling, pulling its
image or running init containers has no log yet. The tail now answers `waiting` until the
container has started, and the page asks again with a backoff (1 to 15 seconds), and at once when
the agent turns ready, so the output appears as the agent comes up. After ten minutes without a
started container it stops asking and offers Try again. Any other failure is still shown.

**Fixed: a person bound to one namespace saw an error on Home and could not open Cost.** Tenants
are cluster-scoped, so a RoleBinding never lets anyone list them, and Home reported that refusal
as an error. The Cost page could not render either: without a tenant the breakdown answered 400.
A person who cannot list tenants is now treated as not a tenant admin. Home leaves out Spending,
the navigation hides Tenants, and the Cost page shows spend in the namespaces where they may read
cost (`/api/cost/breakdown` with no tenant answers for those, and names them in `namespaces`). It
is never cluster-wide for them. Tenant admins see no change: with tenants to see, the breakdown
still needs one. `/api/capabilities` now reports `tenants` (`list`, `create`), probed cluster-wide.

**Fixed: skills did not work on any install.** Neither the BFF nor the controller created the
skills store, so `/api/skills` answered 501 and an agent with `skillRefs` was never deployed: the
controller failed every reconcile with "this install has no skill store". Both now use the
control-plane database. A skill created with `POST /api/skills` resolves for an agent in the same
namespace, pinned to its digest; the console has no skills page yet. Skill bodies are still not
mounted, so `load()` answers 501.

**Fixed: creating an agent on a connection that does not exist made an agent that could never
answer.** A model-first create (`POST /api/agents` with `model.connection`) naming a connection the
namespace does not have built a model route from the connection's name, with a provider guessed
from it and a SecretBinding that does not exist. It is now refused with HTTP 400 naming the
connection, and nothing is created. A client that sends only `model.provider` still resolves the
connection named after that provider.

**Fixed: the Datasets page worked only in the `default` namespace.** The console sent no
namespace with any dataset request, so the BFF read `default`: a person bound only in their own
workspace got 403 on the page, and adding a trace to a dataset or labelling a case wrote to
`default`. Every dataset request from the console now names the selected workspace, and a console
with no workspace selected says so instead of asking. The dataset list also showed a dataset
whose cases could not be read as having 0 cases; it is now an HTTP 500.

**Fixed: a sub-agent asked for the same Connect again after it was given.** A sub-run works in
its parent's trust boundary, so the account a person connects from the chat is stored for the agent
they are talking to. Resuming the sub-run minted its run capability in the sub-agent's own boundary
instead, so for a sub-agent outside any agent registry the credential plane found no grant and the
run paused for consent again. A resumed run's capability is now minted in the run's own boundary,
as the run worker already did.

**Fixed: the chat looked stuck when a sub-agent needed a Connect or an approval.** A supervisor that
delegated waits on its sub-agent's run, and when that run paused for consent or an approval the
supervisor's run event stream only named it (`descendant-requires-action`), which the chat and the
Playground ignored. Both now read the sub-agent's run and show the same prompt as for the agent's
own pause, saying which sub-agent asked. Connect resumes the sub-agent's run, the chat follows the
supervisor on to its answer, and the hold links to the sub-agent's run. If the stream ends while the
supervisor waits, the pause is found from the supervisor's run. When several sub-agents pause
together, the chat asks about them one at a time in the order they paused. Starting a new chat, or
leaving it, while a sub-agent's pause is being read no longer affects the new conversation.

**Fixed: the tool catalog was refused to anyone bound to a namespace.** The console read
`/api/tools` without a namespace, and the BFF then asked whether the caller may list tool registries
in every namespace, which a RoleBinding (the chart's default way of granting a persona) never
allows: the catalog, and the bind wizard behind it, answered 403. The catalog now reads the selected
workspace, and the create-agent tool picker the namespace the agent is created in. "All
workspaces" follows the rule in the next entry.

**Fixed: "All workspaces" was refused to anyone bound to a namespace.** With "All workspaces"
selected, every console list asked Kubernetes, or the access check for the lists kept in Postgres,
about every namespace at once, which a RoleBinding never allows, so each page answered 403. A list
given no namespace now reads the namespaces the caller works in, the set the workspace picker
shows, found once per request with the caller's own token. It answers 200 with an empty list when
the caller may read that kind somewhere and nothing is there, and 403 only when they may read it
nowhere. A caller allowed cluster-wide still gets one cluster-wide read. Paged lists page across
the caller's namespaces. Lists kept in Postgres are read only after the caller is authorized, and
only in the namespaces they may read, so whether the answer is 403 no longer depends on what is
stored: a namespace holding only someone else's private MCP server answers exactly as an empty one.
A namespace where someone may list other kinds but not agents is not part of "All workspaces";
selecting it still reads it. `GET /api/skills` with no namespace now lists the skills of every
namespace the caller may read; it listed none.

**Fixed: some console pages ignored the selected workspace.** The Knowledge Bases, MCP servers, MCP
approvals, Teams, Guardrails and Workflows pages, and the dashboard's run count, sent no namespace,
so they read "All workspaces" whatever was selected. They now send the selection. A team's page
reads its own namespace. The model-route editor offers the providers and bindings of the route's
namespace, and the create flows offer the routes, prompts and registries of the namespace they
create in. The template gallery and MCP catalog read the working namespace, and Fork and Connect
create there; before, all four used `default`, which a person bound to another namespace may not
read.

**Fixed: write controls followed the wrong namespace's permissions.** A knowledge base's Upload
and Ingest, the tool catalog's Bind, the Edit, Delete, Share, Approve and Reject on the rows of the
agent, model route, secret binding, agent registry, eval suite, prompt version, MCP server and MCP
approval lists, the same controls on the agent, model route, secret binding and agent registry
pages, and an agent's chat were offered or hidden by what the caller may do in the shell's working
namespace, not in the namespace the resource is in. Under "All workspaces", or on a page opened
from another namespace, a person could be offered a write they would be refused, or not offered one
they may make. Each now asks about the resource's own namespace. Two tools of the same name and
server in different namespaces now show as two cards.

**Security: a tool binding could name a server its author may not use.** A
`POST /api/mcptoolbindings` that gave `registryRef`, `mode` and `server`, and every
`PUT /api/mcptoolbindings/{ns}/{name}`, were written as sent, so a binding could name another
user's private MCP server, or bind a personal server's tool for someone other than its owner. Every
create and update now checks the named server the way a create that leaves them out does: it must
be one the caller may use and must provide the tool (400 otherwise, the same response whether or
not the server exists), and a personal server binds only for its owner (403). This needs the
control-plane store (501 without it), and the caller's permission is checked before any server is
read.

**Fixed: binding a tool from the catalog always failed.** The bind wizard sent the agent as an
object (`agentRef: {namespace, name}`) and no MCP server, mode or server location, while
`POST /api/mcptoolbindings` takes the binding spec, so every bind returned `400 invalid JSON
body`. The wizard now sends the spec's shape: `agentRef` is the agent's name, the binding is in the
agent's namespace, and `registryRef` is the MCP server the tool was picked from. `registryRef`,
`mode` and `server` may now be left out of a create: the BFF reads them from the tool's entry in the
MCP server that provides it, among the servers the caller may use, refusing a tool no such server
provides with 400 and another user's personal server with 403. Before reading any server it checks
that the caller may create bindings in the namespace (403 otherwise). The wizard no longer offers a binding namespace other than the agent's, which
could only produce a binding to nothing, and offers only agents in the namespace where the tool's
MCP server is registered, whichever workspace is selected.

**Security: an agent could serve a page at its own address and finish a visitor's OAuth consent
as its author.** The console's consent page trusts a page at an agent's hostname, and with the
edge's `extAuth.enabled: false`, `/invoke` at every agent hostname reaches the agent's own
container with no credential. An agent's author could serve a page there that opened a visitor's
consent page with a ticket minted for the author, so the visitor's upstream tokens were stored as
the author's grant. The edge now sets `Content-Security-Policy: sandbox` and
`X-Content-Type-Options: nosniff` on every response from an agent, replacing any the agent sent,
and removes the agent's `Set-Cookie`, so nothing an agent returns through the edge runs as a page
at its address or plants a cookie there, such as a forged OAuth binding cookie; clients reading
`/invoke` with `fetch` or SSE are unaffected. The consent page names the account, server and agent and waits for
Continue, and a ticket is minted only for an agent that exists and that the caller can read
(HTTP 404 or 403 otherwise). **Behaviour change:** connecting from an agent's chat takes one more
click. Re-install the `platform-edge` chart to get the sandbox.

**Security: an OAuth MCP server's authorization endpoint could use any URL scheme.** Registration
and discovery accepted a `javascript:` or `data:` endpoint, and the chat opened whatever address
a consent returned. The authorization and token endpoints must now be absolute `http` or `https`
URLs (HTTP 400 otherwise), discovery refuses metadata that names anything else, and the chat opens
nothing else.

**Fixed: an OAuth connection failed when its callback reached another BFF replica.** Pending
OAuth flows were kept in the memory of the BFF pod that started them, so with more than one
replica (the production overlay runs two) a callback that reached the other pod failed as an
unknown state. Pending flows are now kept in the control-plane database, sealed under a key
derived from their state, and any replica completes them. Their deadlines are set and checked by
the database's clock, so a replica whose clock runs ahead cannot expire another's.

**Fixed: the console never recognised a real tool call in a trace.** Langfuse records the
platform's tool calls as `TOOL` observations, and the run inspector and trace explorer only
treated `SPAN`s as tools. A tool call showed as an internal step, and the run inspector opened on
something else. Both now treat `TOOL` as a tool, and the run inspector opens on the tool call. A
tool call reports no token usage of its own, so it shows no Tokens row; the model call that made
it does.

**Fixed: another user's private MCP server could block or capture your agent's tools.** Creating
an agent bound each tool to the first registered server offering it, by name, whoever owned it.
When that was another user's private server, the create was refused with "tool is on a personal
MCP server you do not own" even though a shared server offered the same tool; an edit silently
re-pointed the tool at the private server. Create, edit and fork now consider only the servers
you can use, and an edit applies the same owner check as a create.

**Fixed: editing someone else's agent re-bound its tools.** An edit resolved every tool again
from the editor's view, so changing only the image on another user's agent moved a tool bound to
their private server onto a shared one, or was refused with HTTP 400 when no shared server offered
it. A tool the agent already has keeps its server, whoever edits. Only a tool the edit adds is
resolved, from the servers the editor can use.

**Fixed: an agent could be created with a tool that does not exist.** A managed agent's tool that
no registered MCP server provides was bound to a default registry that nothing creates, so the
agent ran without the tool and only the binding's `RegistryNotFound` condition said so. Creating
or editing such an agent in the console or the API is now refused with HTTP 400 naming the tool:
add the MCP server that provides it first. Without the control-plane store an agent with tools is
refused with 501. `ctxmesh expand` is unchanged, and forking still creates the agent and flags
the tool for rebinding.

**Fixed: an MCP server registered through OAuth was visible to nobody.** The server is private to
the person who registered it, but it was stored without an owner, so no one could list it, bind
its tools or delete it, including that person. It now belongs to whoever registered it, and a
registration whose user the cluster cannot identify is refused before anything is contacted.
Servers registered through OAuth before this release still have no owner; register them again
under a new name.

**Fixed: an MCP server registered with a key could be stored with no owner.** A keyed server is
private to whoever registers it. When the cluster could not say who that was, the server was
registered anyway, without an owner, so nobody could see it. That registration is now refused with
HTTP 500 before the server is contacted. A server registered without a key is shared with the
team and still registers.

**Fixed: per-version online scores never counted a failed tool call.** The tool-failure count read
only `SPAN` observations, and Langfuse records the platform's tool calls as `TOOL`. A failed `TOOL`
observation now counts. A `SPAN` still counts, for traces recorded before Langfuse typed tool calls.

**Fixed: per-version online scores counted the platform's own failures as failed tool calls.**
Every failed `SPAN` counted, and the agent's launcher records its model gateway calls, AMP calls,
memory and knowledge lookups and async work as `SPAN`s. A failing model gateway raised the
tool-failure count of every run it touched. A `SPAN` now counts only when it is named like a tool
call (`tool:`, `tool_` or `tool ` followed by the tool), the rule the console's trace explorer
uses. Those failures still count as run errors.

**Fixed: every agent run was cut off at five minutes.** Knative's default request timeout is 300
seconds, and the platform never set one, while a run may take 600. A run that sends nothing until
it is done also hit Knative's separate time-to-first-byte limit at the same 300 seconds. Every
agent's revision now carries both, from the new `spec.scaling.timeoutSeconds` (default 600; above
600 requires raising Knative's `max-revision-timeout-seconds`). Existing agents roll one new revision.

**New: `spec.scaling.concurrency` bounds how many runs one replica handles at once.** Unset, it stays
unlimited, as before. When it is bounded, `spec.scaling.targetBurstCapacity` defaults to -1, keeping
Knative's activator in the path to queue runs that find every slot taken. Measured: with concurrency
1 and the activator out of the path, half of a burst was dropped; with it in, none was.

**Fixed: the bundled state layer refused six of the platform's key spaces.** Its ACL admitted
neither run-capability binding nor the proof replay set. So binding failed, and so did the
platform's fast paths for run cancel and the kill switch (cancel still worked; it was just slower),
along with per-user quotas and per-conversation spend. **If you run your own Valkey with an ACL**
(`statelayer.externalAddr`), it must admit `~mem:* ~tenant:* ~a2a:seen:* ~spawn:* ~agent:* ~runcap:*
~run:* ~ns:* ~fleet:* ~user:* ~conv:*`.

**Fixed: a stock install's console could not list a single run.** The console's Runs list read only
from a trace backend (Langfuse), which the chart does not bundle, so on a stock install the list, and
each agent's Runs tab, answered "unavailable". Every install already records runs made through
`POST /api/runs` in its Postgres run store. Without a trace backend, both lists now read from it,
scoped to the agents the caller can see. Cost and tokens still come from a trace backend, so they read
"—" there, never 0. Each row opens the run itself. The Runs page now sends the selected namespace, so
a user whose role is bound to one namespace can list it.

**Fixed: model calls were refused while the gateway restarted.** Every ModelRoute change restarts the
model gateway, and the old pod stopped listening while Kubernetes was still routing to it. Calls in
that second failed with "connection refused"; measured, three or four per restart. A first run made a
minute after creating a ModelRoute, as the quickstart does, failed this way. The gateway now keeps
serving for ten seconds after it is told to stop; measured afterwards, four of five restarts dropped
nothing.

**Behaviour change: a ModelRoute is Ready only once the gateway serves it.** `Ready` used to mean the
route had been written into the gateway's config file. The gateway then took up to a minute to restart
with it, and a call in that window failed with LiteLLM's "Invalid model name", so waiting for `Ready`
did not make a first call safe. `Ready` now turns True (reason `Served`) once the gateway has finished
rolling out the config that holds the route. Until then it is False, with reason `GatewayRolling`, or
`GatewayAbsent` when the gateway Deployment does not exist. Scripts that wait on a route's `Ready`
with a short timeout may need a longer one.

**New: an agent says whether its model route can be called.** An agent that names a route in
`MODEL_ROUTE` carries a `ModelRouteReady` condition mirroring that route's `Ready`. A route the gateway
has never served reports `NotYetServed`; an edited route that the gateway still serves in its previous
version reports `GatewayRolling`. The agent's own `Ready` does not change. The console's chat holds
Send while the route is `NotYetServed`, `GatewayAbsent` or `SecretUnresolved`, and says why. Before,
an agent created in the console answered its first message with "Invalid model name" if the gateway
had not finished loading the new route.

**Fixed: `ctxmesh expand` gave managed agents an image that does not exist.** A managed agent with no
image of its own got `ghcr.io/ctxmesh/managed-agent:latest`, a tag no release publishes, so
`ctxmesh expand agent.yaml | kubectl apply -f -` produced an agent that never started. `make
build-cli` now pins the newest release tag; `MANAGED_AGENT_IMAGE` still overrides; and with neither,
`expand` refuses and says what to set.

**Fixed: durable runs of an agent that does not stream always failed.** The run worker asks agents
for an event stream and read any reply as one. An agent that answered with plain JSON, as the
quickstart's echo agent does, produced "stream ended with no result frame" on every run, from the
console chat and from `POST /api/runs`. A plain reply is now taken as the result, as the execution
contract already said.

**Fixed: an automatic rollback reverted the agent's spec but never rolled the agent.** With
`spec.rollout.autoRollback` enabled, every reconcile after the revert refused the rollback again as
`RollbackFrozen`. That overwrote the `RolledBack=True` condition and ended the reconcile before it
created the reverted version, so the regressed version kept serving and `status.rollback.rolledBackTo`
could name a version that did not exist. A frozen agent is no longer evaluated for auto-rollback, and
an automatic rollback that is refused records why and lets the reconcile continue.

**Fixed: per-version online scores were never recorded.** The launcher labels each trace with
the agent version from `AGENT_VERSION`, and the controller never set it, so every online score was
recorded under no version. Regression detection compares the serving version's scores with the
previous version's, so it never reached a verdict; automatic rollback could not fire on
live data; a canary's two arms were not scored apart; and the console's per-version windows were
empty. Every agent's pod now carries the version it serves, the name in `status.latestVersion`.
Every agent rolls one new revision on upgrade, and an approval carries over to it: a gated agent
that was promoted stays promoted, an agent with a canary moves all its traffic to the new revision
at once, and an agent serving the version a rollback restored keeps serving it. An approval
carries only when the platform alone renamed the revision; a change to the spec, the prompt, a
binding or a skill still waits for its own. A revision created before the upgrade, such as the old
arm of a canary in progress, still records no version until it is replaced. An `AGENT_VERSION` set
in `spec.env` is not used (see the entry on identity variables).

**Fixed: an aborted canary reopened when its candidate's revision was renamed.** An abort held only
while the candidate kept its revision name, which also changes on a platform upgrade such as this
one, so the split reopened at `canaryPercent` to the rejected candidate. It now holds until the
spec changes or someone promotes the candidate.

**Fixed: reverting the spec after an aborted canary sent most traffic to the rejected candidate.**
When no promote annotation named the revision the abort kept serving, as after an automatic
promotion, the revert opened a split with `100 - canaryPercent` percent on the candidate that was
withdrawn. A spec that matches the revision already serving all traffic now opens no canary and is
recorded as promoted, and an automatically promoted revision no longer drops to
`awaiting-promotion` on the next reconcile.

**Fixed: a rollback left most traffic on the revision it rolled back from.** On an agent with a
canary, the version a rollback restored opened as a new canary candidate, with `canaryPercent` of
the traffic, and after an automatic rollback the freeze held that split while `RolledBack=True`. A
gated agent without a canary held the restored version at `awaiting-promotion`. The version a
rollback restores now takes all traffic at once, with no canary, approval or eval-gate run. Only
the spec the rollback writes is served this way, and `status.rollback.revertedGeneration` records
which one that is: any later spec change, including an edit back to the restored version after the
version it replaced has been deployed again, canaries and gates as before.

**Fixed: re-applying a version that was rolled back promoted it at once.** A rollback left the
`agents.ctxmesh.ai/promote` annotation that approved the version it rolled back from, so applying
that version's spec again promoted it immediately: all traffic, with no canary and no wait for
approval. A rollback now removes a promote annotation that names a revision of the version it rolls
back from, in the same write that reverts the spec.

**Fixed: connecting a provider that was already connected failed with HTTP 400.** Every release
updated the provider's SecretBinding and ModelRoute without a `resourceVersion`, which the API
server refuses for a custom resource, so connecting a provider again to change its key or endpoint
always failed. It now updates them in place. Connect also refuses, with 409, a connection name that
belongs to a ModelRoute or SecretBinding it did not create, rather than replacing that object.

**Fixed: the console's Rotate key action was refused for every role.** It read the provider's
Secret, which no shipped role may do, so it answered 403 while the console offered the button. A
rotation now validates the new key against the endpoint the provider's model route calls, then
rewrites the provider's Secret, SecretBinding and route, keeping the route's endpoint, model and
rate limit. It never reads the key it replaces, and the audit log records it as a `connect` with
`rotate: true`.

**Fixed: the console could open in the control-plane namespace, and the config builder ignored the
selected workspace.** On a first visit the console selected the first namespace it could list. For
a cluster-wide user that was usually the control plane's own namespace or a `kube-*` one, and
connecting a provider there wrote the key into the control plane. It now opens in the first
namespace that is not a platform namespace, or in All workspaces; a stored choice still wins. The
config builder's Apply created the agent in `default` whatever workspace was selected. It now
applies into the selected workspace, and under All workspaces asks for one first.

**New: an agent's page lists what it references, and says which ones resolve.** A Bindings
panel shows the model route and the secret bindings it uses, the guardrail policy, tool bindings,
knowledge bases, memory, the tool calls that wait for approval and who may approve them, and the
feedback sources, each looked up as you. A reference that does not exist is shown as missing.

**The Playground is a chat.** It used to ask for the message as JSON. Pick an agent, type a
sentence and read the answer; the message is sent as `{"input": "..."}`, as on the agent's own
page, and Send waits while the agent's model route is still loading. A JSON request is one click
away for an agent that takes structured input. Both keep the trace link and now show how long the
run took. An agent picked under All workspaces runs in its own namespace, not in `default`.

**Home's "Run your agent" step opens a chat with the agent you just created.** It used to open an
empty Playground. It now opens the Playground with your newest agent picked; the agents list
carries each agent's `createdAt` for this. A chat on the agent's own page also checks the step off.

**Breaking:** with a state layer present (the default), the BFF refuses unbound capabilities.
- **Older agents:** an agent built on an older base image has a launcher that cannot bind, so those
  calls fail until it is rebuilt. To run older agents meanwhile, set
  `bff.runCapabilities.requireProofOfPossession=false`; a copied capability can then be spent.
- **AMP callees:** an agent called over AMP can no longer spend the capability its caller relayed.
  That capability belongs to the caller's run, so spending it acted as the caller.
- **Guarded AMP callees:** a guarded agent called over AMP authenticates to the audit edge with its
  own pod token (BFF audience), since the capability it holds is its caller's. Its durable block
  record names the user from the capability and the agent from the pod. The BFF gains one
  permission for this: `create` on `tokenreviews`.

**Breaking: four resource kinds are retired, and their settings move onto what they configured.**
The API now serves 14 kinds instead of 18 (ADR 0152). Move any objects before you upgrade.
`helm upgrade` refuses while any object of the four kinds still exists, and names each kind and
how many. The guard relies on Helm `lookup`, which `helm template`-based tools such as Argo CD do
not run, so their users must check `kubectl get approvalpolicies,feedbackstores,credentialstores,clustercredentialstores -A`
themselves before upgrading:

- **`FeedbackStore` → the agent's `spec.feedback`.** Copy the store's `spec` (`mode`, `human`,
  `external`) into the agent that named it in `spec.feedbackStoreRef`, and delete that field. The
  API server now rejects a declaration with no source, or with a score name used twice.
- **`ApprovalPolicy` → the agent's `spec.runtime.toolPolicy`.** `rules[].allTools` becomes
  `default: require-approval`, `rules[].tools` becomes `overrides` entries with
  `rule: require-approval`, and `approvers` moves to `toolPolicy.approvers` (an approver must be
  able to get the agent; empty means anyone who can act on the run and get the agent). Delete
  `spec.approvalPolicyRef`. One meaning changes: an explicit `allow` override now wins over
  `default: require-approval`, because there is no second policy to merge with. Editing
  `approvers` or `spec.feedback` does not create a new agent version or roll the agent, and
  version snapshots no longer record either. A rollback keeps the agent's current approval rules
  (`default`, `overrides`), `approvers`, `maxToolCallsPerRun` and `feedback`, and restores
  `forcedChoice` and `parallelLimit` with the rest of the version; a rollback that would change
  nothing else is refused as `RollbackNoEffect`.
- **`CredentialStore` and `ClusterCredentialStore` → Helm values `tokenService.credentialBackend`.**
  Set `type` to `kubernetes`, `postgres` or `remote`, and move the store's `spec.provider.<type>`
  block under the matching key. Two keys do not move. `kubernetes.credentialNamespace` has no
  `credentialBackend` key: the namespace is `bff.mcp.credentialNamespace`, the existing value the
  token-service reads as `MCP_CREDENTIAL_NAMESPACE`, so an install that set `credentialNamespace`
  must set that value to the same namespace, or its grants are orphaned. `kmsV2.keyIDPrefix` is
  gone: KMS v2 is single-key, and the prefix was never used. The never-implemented `openbao`
  provider is gone, and so is the per-namespace override. If you created a `ClusterCredentialStore`
  named `default`, the token-service used it, and you must set these values to keep that backend;
  otherwise it runs on `kubernetes` and your users reconnect their accounts. The documented
  `cluster-default` name was never read, so those installs were already on `kubernetes`. A missing
  or malformed value stops the token-service at startup with an error naming the setting. Its
  ClusterRole is removed.

Helm does not delete CRDs. After moving any objects, delete the retired ones:

```sh
kubectl delete crd approvalpolicies.agents.ctxmesh.ai feedbackstores.agents.ctxmesh.ai \
  credentialstores.agents.ctxmesh.ai clustercredentialstores.agents.ctxmesh.ai
```

**Breaking: agent version history moves to the control-plane database, and `AgentVersion` is
retired.** The API serves 13 kinds (ADR 0042). The controller recorded every spec change as an
`AgentVersion` object and never deleted one; it now records each version in Postgres and keeps an
agent's 20 newest, plus any still in use (the serving version, the one before it, and every
version `status.rollback` names). Rollback is unchanged: annotate
`agents.ctxmesh.ai/rollback=<version>`, or click Roll back in the console.

- **Upgrading needs no preparation.** On its first start the new controller copies the existing
  `AgentVersion` objects into the database, oldest first, before it reconciles anything. Objects
  whose agent was deleted, or deleted and recreated, are skipped, as garbage collection would have
  removed them. The controller logs `Version history import complete` with the counts. Avoid spec
  changes while the upgrade rolls out: a version the old controller records after the new one has
  started is not copied. After checking an agent's history in the console, delete the old kind
  (Helm keeps CRDs): `kubectl delete crd agentversions.agents.ctxmesh.ai`.
- **The console's version list is newest first.** It was in name order, so Compare's default pair
  (previous and latest) was effectively random. This is the only change to the API's wire format.
- **Reading version history takes `get` and `list` on `agentversions`.** The BFF checks them as the
  caller, so the built-in `viewer`, `developer` and `operator` roles keep them. A custom role
  without them sees an empty version list and gets 403 from the version diff. The scaffolded
  `agentversion-admin-role`, `agentversion-editor-role` and `agentversion-viewer-role` are removed.
- **A database outage does not block a deploy.** If a version cannot be recorded, the agent rolls
  anyway, shows `VersionRecorded=False`, and the controller retries every 30 seconds and counts the
  failure in `ctxmesh_agent_version_record_failures_total`. Every agent now carries a
  `VersionRecorded` condition. A rollback requested while history cannot be read waits with
  `RolledBack=Unknown` (reason `VersionStoreUnavailable`) and keeps its annotation, and the agent
  still rolls whatever spec you apply.

**`AgentTeam` and `Workflow` are frozen.** Both stay, but neither gains a field until the install
path is green (ADR 0152). CI fails on any change to their generated CRDs unless
`hack/crd-freeze.sum` is updated on purpose.

**Breaking: the API serves only `v1beta1`.** `agents.ctxmesh.ai/v1alpha1` is no longer served for
any kind (ADR 0100). Author manifests at `agents.ctxmesh.ai/v1beta1`; the fields are identical, so
the `apiVersion` line is the whole change. `ctxmesh expand` and the console emit `v1beta1`, and Go
code that imported `github.com/ctxmesh/ctxmesh/api/v1alpha1` imports `.../api/v1beta1` instead.

Every published release already stores `v1beta1`, so an upgrade rewrites nothing. A cluster whose
CRDs list `v1alpha1` in `status.storedVersions` cannot take the new CRDs, because Kubernetes
refuses to remove a version still listed there. Only development builds from before
`v1beta1` became the storage version are affected. To check, and to migrate one kind before
upgrading:

```sh
kubectl get crd -o custom-columns=NAME:.metadata.name,STORED:.status.storedVersions | grep ctxmesh
kubectl get agentdeployments.agents.ctxmesh.ai -A -o json | kubectl replace -f -
kubectl patch crd agentdeployments.agents.ctxmesh.ai --subresource=status --type=merge \
  -p '{"status":{"storedVersions":["v1beta1"]}}'
```

**Behaviour change: the bundled NATS is off by default.** Nothing used it until `bff.asyncBackend`
was set, so every install ran an idle broker on a 4 GiB volume. Turn it on with
`devDataPlane.nats.enabled=true`, `bff.asyncBackend=jetstream` and
`bff.natsURL=nats://ctxmesh-nats.<namespace>.svc:4222`. An upgrade removes the NATS Deployment and
its PersistentVolumeClaim; if you use the bundled broker for async calls, set
`devDataPlane.nats.enabled=true` before upgrading, or messages still queued in it are lost.

**The bundled object store can be turned off.** `devDataPlane.objectstore.enabled=false` leaves
SeaweedFS out, and the BFF and the controller are no longer handed its address: knowledge-base
upload and ingestion and the run-fixture view answer 501, record-capable agents fail at start with
`OBJECT_STORE_ADDR is unset`, and async payloads between registry members travel inline (up to
1 MiB) instead of being offloaded. It stays on by default. With `devDataPlane.enabled=false` the
BFF's `OBJECT_STORE_ADDR` is now empty too, so those endpoints answer 501 instead of failing on a
Service that was not rendered.

**Every third-party image can be moved to another registry, and all of them are pinned by digest.**
LiteLLM, Valkey, PostgreSQL, SeaweedFS, NATS and Dex are under `thirdPartyImages`, each as
`registry`, `repository`, `tag` and `digest`; `thirdPartyImages.registryOverride` moves all of them
at once, and `global.imagePullSecrets` adds pull secrets to every pod the chart renders. LiteLLM
(`v1.91.0`), Valkey (`8.1.3`) and Dex (`v2.41.1`) were pinned by tag and are now pinned by digest;
the images are unchanged. **Breaking for one value:** `auth.oidc.image` moved to
`thirdPartyImages.dex`, and a render that still sets it fails with a message naming the new key.
CI and the nightly pull every third-party image the chart renders without credentials, so an
upstream that closes anonymous pulls fails a check rather than an install.

## v0.1.0-beta.9 — the first install stopped working, and the gate that would have said so had never run

**Every new install of `v0.1.0-beta.8` fails.** The bundled dev object store is MinIO, and MinIO
closed anonymous access to their container images. `devDataPlane.enabled` defaults to `true`, so a
stock `helm install` renders it, the image cannot be pulled, and the install dies:

```
INSTALLATION FAILED: Deployment/ctxmesh/ctxmesh-objectstore not ready
quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z@sha256:14cea493… → unauthorized
```

Nothing we shipped caused it; the identical chart was green days earlier. Verified rather than
assumed before concluding: quay.io by digest, by tag, three older releases and `:latest`, plus
Docker Hub — every one denied.

**This is the second occurrence.** Docker Hub closed first, which is why the image had already
moved to quay.io. Chasing a third registry would be the same bet a third time, so the bundled
store is now **SeaweedFS** (Apache-2.0, multi-arch, pinned by digest). S3 stays on port 9000, so
the Service, the NetworkPolicy and every `OBJECT_STORE_ADDR` are unchanged — and **the client did
not change at all**: `minio-go` is a Go module, immune to a container-registry lockdown. If you run
with `devDataPlane.enabled=false` and your own S3, nothing here affects you.

The object store also now runs non-root (uid 10001, capabilities dropped, seccomp `RuntimeDefault`).
It previously had no `securityContext`, so restricted namespaces warned on every apply.

**A reachable CVE is fixed.** `GO-2026-6505` — the OpenTelemetry OTLP exporter could leak endpoint
URLs into info logs, reachable from the launcher and the BFF. Bumped to v1.45.0.

**Security: a custom provider's key was sent to OpenAI.** An agent created in the console on a
custom (OpenAI-compatible) connection got a model route without the connection's endpoint, so its
calls went to the provider type's default host, `api.openai.com`, carrying the key you gave the
custom connection. Routes now keep the connection's endpoint, and a route created before this fix is
repaired the next time an agent is created on it. **If you connected a custom provider, rotate that
key.**

**Agents created in the console never started.** The console creates a managed agent from
`bff.managedAgentImage`, which was empty, so the BFF used its compiled-in
`ghcr.io/ctxmesh/managed-agent:latest`, a tag no release publishes. Every such agent waited on an
image that does not exist. The chart now pins it to the release's own version, like the images it
injects, and `release-truth` checks the value.

**Why it took eight days to notice.** The nightly job that walks a stranger's path — cold cluster,
published artifacts, public docs — had failed 30 nights running, every run dying in a preflight
step before it ever reached the install. Its own header declared it "expected red", so nobody read
it. Both are fixed: the job now runs, and there is no expected-red state.

## v0.1.0-beta.8 — the controller stopped reconciling, and nothing said so

A one-line RBAC fix, cut as its own release because the version it replaces is known-broken.

**The control plane could stop reconciling entirely, silently.** The prompt-ConfigMap garbage
collector lists Knative `Revision`s so it never prunes a ConfigMap a live revision still mounts —
and the ClusterRole granted `serving.knative.dev` → `services` only. The consequence is worse than
a denied call: controller-runtime's *cached* client blocks waiting for an informer to sync, an
informer without `list`+`watch` can never sync, and with a worker count of 1 the first agent to
reach that line wedged the `agentdeployment` controller permanently. Every agent after it stopped
reconciling too.

Nothing in the usual places said so. The pod was Ready with zero restarts, `Starting workers` was
logged for the controller, and no error mentioned the object. The only honest signal was
`status.observedGeneration` sitting empty on a freshly created agent.

If you are on `v0.1.0-beta.7` or earlier and any agent uses `promptRef`, upgrade. If your agents
have stopped picking up changes — a prompt swap that never reaches the pod is the usual first
symptom — this is why.

**A prompt swap reaches the serving revision again.** With the grant in place, changing
`spec.promptRef` rolls a new Knative revision mounting the new prompt, with a byte-identical
container image digest.

**The console tells the truth in two more places.** The Home checklist's "Run your agent" step now
checks off after a Playground run even where the trace feed is empty — on a default install it is
ready and empty, so the step could never complete. And a trace whose spans have not arrived yet
reads as *still recording* rather than "The trace didn't load": the backend returns the same 404
for a trace that will never exist and one that is seconds away, and the two are now distinguished
on the wire.

**Also fixed:** the `awaiting-promotion` condition told operators to annotate
`agents.ctxmesh.ai/promote=true`, which stopped working when approval began naming the candidate
revision it authorizes. Following the product's own instruction did nothing, and the condition then
repeated it.

## v0.1.0-beta.7 — the install works where nobody prepared the ground

The first release cut because a gate walked a stranger's path and refused to go green.

**The documented first command failed, and now does not.** `helm install --create-namespace` — copied
verbatim from the getting-started page — could not complete on a cold cluster: the chart templated its
own release namespace carrying `managed-by: kustomize`, so Helm could not adopt the namespace it had
just created. The chart no longer owns that namespace, which is also what Helm prescribes:
`--create-namespace` deliberately creates an *unowned* namespace so `helm uninstall` can never delete
it, taking the state-layer PVCs with it.

**No agent could start.** Every agent pod runs three controller-injected sidecars, and beta.6's
release matrix had eight entries — `agent-otel-collector`, `agent-discovery` and `egress-sidecar` were
never built at all. They are in this release. Separately, the base images ran as root while the
controller sets `runAsNonRoot`, so the kubelet refused the container outright; the published
managed-agent reported `User=[]` where the repaired build reports `65532:65532`.

**The control plane crash-looped on the documented prerequisites.** A cluster with Knative *Serving*
and nothing else could not start the manager: it hard-watched Knative Eventing and KEDA types that
were not there, failed its cache sync, and exited — while `helm install --wait` reported success,
because the crash comes after the first startup window.

**A tool call is proven, not assumed.** The stock managed loop silently dropped tool calls whenever a
token sink was wired: the model gateway streams, and a non-streaming upstream proxied into SSE emits
an empty delta, so the loop saw a turn with no content and no tool calls and called it the final
answer. A turn with neither is now a failed turn rather than an empty answer, and the loop reports how
many tools it discovered and how many the model asked for — diagnostics that had never reached a log,
because nothing in the runtime configured logging at all.

**Traces arrive as one tree.** The example agent never adopted the inbound `traceparent`, so every span
it emitted became its own root and a single run showed up as five sibling traces. The launcher's
forwarding memory path emitted no spans whatsoever, so on a durable-store deployment every memory
write was invisible.

**Console and docs.** A retired CRD's console surface — five calls to a route the BFF stopped serving,
every action gated on a capability string that can no longer exist — is gone. The helm-values
reference no longer names a value the chart does not have.

Found by `make -C harness stranger`: a cold cluster, the published artifacts, the public docs, to a
Ready agent, a tool call, and a visible trace. It reports `FIRST CONTACT: 1097s from an empty cluster
to a reconciled agent`.

## v0.1.0-beta.6 — the release reports itself

The first release verified by a gate instead of by hand.

**One command now answers "what actually got published?"** `hack/release-truth-sdks.sh` asks all six
registries plus the chart and fails on anything absent. [ADR 0139](https://github.com/ctxmesh/ctxmesh/blob/main/decisions)
asked for this after beta.2 shipped 8 of 9 artifacts and it was never built; establishing the truth
took six manual queries as recently as beta.5. Two things made it harder than a loop over six URLs:
every ecosystem spells the version differently (`0.1.0b6` on PyPI, `0.1.0.pre.beta.6` on RubyGems),
and a registry can answer 200 and still be wrong, so each probe asserts the **version**, not the
package.

**CI stops failing on someone else's outage.** `proxy.golang.org` drops HTTP/2 streams under load and
took down two different required jobs 24 minutes apart during beta.5, neither related to the change
under test. All 11 Dockerfiles now retry `go mod download`, and the `go install` paths get a
workflow-level `GOPROXY`. The cost was never the re-runs — a required check that goes red for
unrelated reasons teaches you to dismiss red.

**Maven no longer blocks on Central's queue.** The publish job waited for full publication, which took
14 minutes for beta.4 and was still waiting at 25 for beta.5, where the job timeout killed it *after*
the bundle had uploaded successfully. It now waits only for validation — which still fails on a bad
signature, a missing javadoc or a malformed POM — and Central publishes on its own schedule.

**npm silently truncates a description at 255 characters.** The TypeScript description was 342, so it
published cut mid-word, losing the repository URL at the end. Every local check passed because the
file was correct; only the registry knew.

**The four plane-client SDKs have documentation.** `/sdk/go/`, `/sdk/rust/`, `/sdk/ruby/` and
`/sdk/java/` were all 404 — four SDKs on public registries with no guide. Each page is written
against that SDK's real exported surface and states plainly what the plane-client tier does not
include.

**The public site can no longer drift unnoticed.** A new harness gate asserts every version named in
an install command is installable — it runs at *release*, not on the docs deploy path, so a registry
hiccup cannot block a docs deploy. It immediately found the install pages pinning a chart a release
behind.

Also: `npm install ctxmesh` still resolves to an older build than other ecosystems, and that is now a
recorded decision rather than an oversight — npm's OIDC trusted publishing cannot move a dist-tag,
and the token it would need cannot be scoped to that one operation. See ADR 0140. It self-heals at
the first stable release.

Upgrading from beta.5 needs no action — release tooling and documentation only, no runtime change.

## v0.1.0-beta.5 — the description says what ctxmesh is, and where it lives

beta.4 fixed the SDK READMEs. That was the wrong surface for two of the six registries, and this
release finishes the job.

**rubygems.org and Maven Central render no README at all.** Their page body is the gem
`description` / POM `<description>`, so the footer added in beta.4 was invisible there — measured on
the published beta.4 gem, `rubygems.org/gems/ctxmesh` showed none of it. A reader saw "typed clients
for agents running on ctxmesh", with no way to learn what ctxmesh *is* or where the project lives.
The repository link existed only in a sidebar list.

**None of the six descriptions mentioned the repository, and none said what ctxmesh is.** Every one
now opens with "SDK for ctxmesh, the Kubernetes-native control plane for AI agents" and ends with
`Source, docs and issues: https://github.com/ctxmesh/ctxmesh`. Go's package doc — what pkg.go.dev
renders — says the same.

**The gate was blind in the same place.** `hack/sdk-readme-truth.sh` checked READMEs, which is why
beta.4 passed it while two registry pages still told a visitor nothing. It now also asserts every
package's *description* references the repository, and was proven by removing the reference and
watching it fail.

Upgrading from beta.4 needs no action — metadata only, no code changed.

## v0.1.0-beta.4 — the registry pages tell the truth

beta.3 published six SDKs. This release fixes what those six pages actually said, because a
package nobody can orient themselves around is not really published.

**Four SDKs never linked the project.** Go, Rust, Ruby and Java were built from a shared template
that dropped the footer Python and TypeScript carry, so someone landing on crates.io or Maven
Central found a package and no route back — no repository, no docs, no issue tracker. All six now
end the same way: SDK overview, compatibility, source and issues, and a link into the SDK's own
directory.

**Java's install snippet did not work.** It pinned `0.1.0-beta.1`, which 404s on Maven Central, so
the copy-paste install failed outright. Rust pinned the same version; Cargo resolves it through
caret semantics, so it *worked* while telling people the wrong version — the quieter failure, and
the one that survives longer. Both now pin the shipped version.

**The cause was structural, and is closed.** [ADR 0135](https://github.com/ctxmesh/ctxmesh/blob/main/decisions)
stamps Python and TypeScript at release; the other four ship verbatim, whatever is in the tree, and
nothing wrote them. `hack/bump-version.sh` is now the single writer for those four manifests *and*
the README pins, with `--check` in CI. `hack/sdk-readme-truth.sh` additionally asserts every README
links the repository, pins a version that **actually resolves on that SDK's own registry**, and
offers no documentation link that 404s — a live check, because Java's pin was perfectly well-formed
and still a 404.

**The docs site named two SDKs while six shipped.** Go, Rust, Ruby and Java appeared nowhere on
ctxmesh.github.io. The SDK index now carries all six with their registries and explains the two
tiers, and the compatibility table has six rows. Both Helm install pages had also drifted two
releases behind and now pin the current chart.

**Pre-release installs are documented because they are not uniform.** `pip install ctxmesh` takes a
pre-release when no stable exists; `gem install ctxmesh` refuses and needs `--pre`, though a
Gemfile's `gem "ctxmesh"` resolves it without help; npm uses the `beta` dist-tag; Maven and Cargo
take an exact version. RubyGems publishes `0.1.0-beta.4` as `0.1.0.pre.beta.4`.

Upgrading from beta.3 needs no action — no runtime behaviour changed.

## v0.1.0-beta.3 — the install stops needing a flag

beta.2 made a cold install *possible*. This makes it work without the user knowing to ask.

**The control plane waits for its store instead of exiting.** `controller`, `bff` and
`token-service` fail-fast when `CONTROLPLANE_DSN` is unreachable and rely on Kubernetes to
restart them. That stance is preserved — a store that dies while a process is serving still
takes it down — but it was never about start-up *order*. CrashLoopBackOff is exponential to
a 300s cap, so on a cold cluster where PostgreSQL is still pulling its image, its dependents
burn restarts and are asleep in a five-minute backoff by the time the database is healthy.

Only the start-up connection retries, backoff is capped at 15s (an *uncapped* exponential is
the shape that caused this), and `CONTROLPLANE_STARTUP_TIMEOUT=0` restores the previous
behaviour exactly.

**`--timeout` is still required on a cold cluster, and this does not change that.** The
retry removes the *backoff* — the part that made a first install a coin flip decided by
which image finished pulling first. It does not remove the *pulling*: the PostgreSQL image
alone took 4m31s in a measured run and a full cold install took 8m05s, both past Helm's
five-minute default. What changes is that the install now fails or succeeds for an honest
reason instead of a race. Keep `--timeout 20m` on a first install; a chart cannot set that
default on your behalf, because Helm has no chart-side equivalent of the client flag.

**A release gate that runs.** `make release-verify VERSION=v<x.y.z>` installs a *published*
release onto a cold cluster from `oci://ghcr.io`, pulling the chart logged-out. The guard
that caught beta.1 being uninstallable existed but nothing ran it, so it would not have
caught the next one.

**CI stopped lying.** Three test packages dropped a shared `knowledge_chunks` parent in the
one database twenty-four test files share, and `go test` runs packages in parallel — so one
package's setup deleted another's partitions mid-test. It presented as flakiness: the same
commit passed `unit` on a PR and failed it on `main`, which is what makes a green check stop
meaning anything. Each package now gets its own Postgres schema.

Upgrading from beta.2 needs no action.

## v0.1.0-beta.2 — the first install actually works

v0.1.0-beta.1 published correctly and could not be installed. This release fixes that, and
it is the first version whose install was tested from the published artifacts *before*
shipping rather than after.

**The command on the release page could not succeed.** It was `--wait` with no `--timeout`,
which is Helm's 5-minute default. A first install pulls nine images into an empty cache and
the PostgreSQL image alone took 4m31s in a measured cold run. That is not bad luck, it is
arithmetic — the command budgeted five minutes for work that needs fifteen, then reported a
failed install while the cluster was still pulling. The release notes and the install docs
now pass `--timeout 20m` and explain that it is a ceiling, not an expected wait.

**Two Helm hooks were timing the image pull, not their own work.** `activeDeadlineSeconds` is
counted by Kubernetes from Job *creation*, so the capability-keygen hook's 120s covered the
pull of the image it had not yet started. On a cold node it lost that race and
`backoffLimit: 0` made the loss terminal. `backoffLimit: 0` is kept — its reasoning, that a
keygen which *ran* and failed must not retry into an "already provisioned" no-op, is sound and
was simply never about a container that failed to start. Both hooks now allow 900s and are
configurable for a slow registry.

**The control plane slept through its own dependency recovering.** `controller`, `bff` and
`token-service` exit when the control-plane store is unreachable and rely on Kubernetes to
restart them, but CrashLoopBackOff is exponential to a 300s cap. While PostgreSQL pulled, its
dependents accumulated backoff and were still asleep after it was healthy — observed directly
as `postgres 1/1 Running` beside `controller CrashLoopBackOff` — tripping the 600s
`progressDeadlineSeconds` default with nothing actually broken. That bound is now 1800s, which
hands the limit back to the user's `--timeout`.

**Why this was invisible.** Every tier installs from a working tree or a local registry that
serves images in about a second, so every deadline was met with room to spare. A new harness
slice installs from `oci://ghcr.io/ctxmesh/charts/ctxmesh` onto a cold cluster, pulling the
chart *logged out* — an authenticated pull succeeds against a private package, which is how
beta.1 was called verified while five of six packages were unreachable. It asserts what a
pods-are-Running check waves through: Helm status `deployed` at revision 1, and the capability
keypair Secret existing.

Upgrading from beta.1 needs no action beyond the new `--timeout`. If a beta.1 install is
sitting at `STATUS: failed`, re-running `helm upgrade` provisions the keypair it never created.

## v0.1.0-beta.1 — the first beta

The first release you can install without cloning the repository.

Everything before this was verified from a working tree — `kustomize build config/`,
images built locally and loaded into kind. That is the right thing for a dev loop, and it
is why a set of defects survived until a release forced the question: the chart's
first-party images were bare names (`docker.io/library/controller:latest`), `appVersion`
was `latest`, the release workflow published one of five images and no chart at all, and
the chart's post-install preflight was denied by the datastore NetworkPolicy on every
cluster that enforces one. All fixed here (ADR 0133).

### Install

```sh
helm install ctxmesh oci://ghcr.io/ctxmesh/charts/ctxmesh \
  --version 0.1.0-beta.1 --namespace ctxmesh --create-namespace --wait
```

Upgrades: [docs/upgrading.md](docs/upgrading.md).

### What a beta means here

The platform runs agents on Kubernetes: an `AgentDeployment` CRD, a managed runtime that
needs no Dockerfile, a model gateway, per-tenant isolation, RAG over a bundled pgvector
store, evals, guardrails, cost attribution, and a console that goes from an idea to a
running agent without touching YAML.

**Beta means the API is `v1beta1` on purpose** (ADR 0100) — the shape is settled enough
to build on and not yet frozen. Breaking changes will come with a documented migration.

### Highlights since the last milestone arc

**Authoring.** The console's create path is proven end to end by a single test that walks
it: connect a provider, author an agent in form fields, watch it reach Ready, find it
again — no YAML anywhere. That walk found four defects no per-screen test could see,
including a connect wizard offering providers the API rejected, and a permission check
that refused *after* the user's API key had been submitted.

**Providers.** Anthropic, OpenAI, and any OpenAI-compatible endpoint (`custom`, with a
required base URL). Gemini connects through the custom provider with base
`https://generativelanguage.googleapis.com/v1beta/openai`.

**Knowledge.** A second modality (scanned PDFs via OCR), a golden-embedding check that
catches a silently swapped embedding model, and concurrent ingests refused rather than
allowed to destroy each other's chunks.

**Security.** Datastore NetworkPolicies, PSA labels on the install namespace, RBAC
narrowed to namespace-scoped Secret writes, and a least-privilege gate that fails the
build on verb wildcards or cluster-scoped Secret writes.

**Install.** Postgres and NATS are bundled and chart-owned; an install-truth gate fails
the build when the chart consumes something it never creates.

### Breaking change: the inter-agent telemetry namespace

The inter-agent call surface is now called **AMP**, renamed from A2A — which predated Google's
Agent2Agent by years and had come to collide with it while meaning the opposite thing (theirs is
interop between agents run by different parties; ours is mediation between agents one platform
already owns).

Almost nothing about the rename is breaking. `client.mesh.call()` is unchanged, the launcher still
serves `POST /a2a/{target}` alongside `/amp/{target}`, and it still sends **and** accepts
`X-A2A-Envelope` alongside `X-AMP-Envelope`, so an older SDK keeps working against a newer launcher.

**What does break: spans, span events and attributes move from `a2a.*` to `amp.*`** — `amp.call`,
`amp.guard`, `amp.guard_tripped`, `amp.cross_registry_denied`, `amp.conversation.id`, and the
`amp.async.*` family. A span cannot be emitted under two names at once, so this is one deliberate
cut. **A saved dashboard, alert or trace query keyed on `a2a.*` returns nothing rather than
erroring** — grep your saved queries before upgrading. Full mapping in
[docs/upgrading.md](docs/upgrading.md).

### Prerequisite clarified: Knative Eventing is required

The controller watches Knative Eventing `Trigger` resources at startup, so on a cluster without
Eventing it cannot start — **even if you only ever use the serving execution model**. `Chart.yaml`
has always listed it; the install docs did not, and now do. Install Knative Serving *and* Eventing
before the chart.

### Also in this release

- **Home is a work queue.** The console's landing page leads with one ranked list of everything
  blocked on a person — stops, approvals, failing agents, critical alerts — over a fleet bar whose
  every stage opens the list it counts. Counts that used to read "not yet known" above 200 agents
  are now real, from a new census endpoint.
- **Counts that were wrong are fixed.** The alerts feed answered a cluster-wide read with zero
  (both stores filtered on namespace equality while the console sent none), and a capped fetch was
  printed as a total. Any count that could be the size of a page now renders as a bound.
- **Zero reachable vulnerabilities.** Seven standard-library CVEs and one in gRPC, all reachable
  from the call graph, closed before this tag.
- **The durable stores are tested against a real database in CI**, not only against their in-memory
  test doubles.

### Known limitations

- **Connecting a provider needs a permission no shipped role grants.** `secrets: create`
  is deliberately withheld from `ctxmesh-operator` for least privilege, so a cluster
  admin must grant it before anyone can connect a provider in the console. The console
  tells you this up front rather than failing after your key is submitted. A
  provider-admin role is the next release's work.
- **Audio and vision ingestion** are implemented but unproven end to end — they need
  models the test clusters do not carry.
- **The BFF's memory grows across ingestions.** Large corpora may need a raised memory
  limit; the leak is tracked, not papered over with a bigger default.
- **Contributions open with this release** under the DCO (ADR 0134) — `git commit -s`.
