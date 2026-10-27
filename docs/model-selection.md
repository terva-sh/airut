# Choosing an acceptable model for each request

Configure independent ordered candidate lists with `[agent].models` and
`[agent.judge].models`. Before each physical inference attempt, airut scans
all candidates for a ready model, breaking ready ties by list order and
skipping open circuit breakers. An unloaded earlier candidate cannot
outrank a later ready one.

Only when **every possible route of every acceptable candidate** reports
`unloaded` may airut consider a cold load. It chooses the first eligible
candidate whose backend policies permit it. A ready candidate behind an
open breaker still prevents loading another candidate. Missing models,
missing routes, empty catalogues, failed discovery, `loading`, and
`unknown` never prove absence. A backend's `never` restriction wins across
provider aliases that identify the same backend.

A streaming response finishes on its selected model. Completed tool results
stay in the transcript; the next request can select another model without
rerunning tools. Each engine request is configured with the selected
candidate's context window, output limit, image capability, served ID,
temperature and timeout. A conservative input estimate (serialized request
bytes divided by three, plus reserved output) prevents sending a request
over its configured context cap. An oversized candidate is skipped for
another acceptable ready candidate; nothing is silently truncated. Images
omitted for a non-vision request remain available in the live transcript.

## Application discovery

Implement `agent.Discovery` in the application. airut does not parse an
application's sidecar HTTP protocol:

```go
func (d *DiscoveryAdapter) Snapshot(ctx context.Context, keys []agent.ModelKey) (agent.Snapshot, error)
```

`ModelKey` is `(Provider, Served)`, after configuration aliases resolve.
For each key, `Snapshot.Routes` must enumerate **all possible inference
routes**, each an `agent.Route{Backend, Served}`. The backend's served ID may
differ from the public inference ID. `Snapshot.Backends` maps stable backend
identities to `BackendState{Models, Idle}`. `Models` maps each backend-served
identity to `StateReady`, `StateLoading`, `StateUnloaded`, or `StateUnknown`.
All routes must be ready for a ready send. There is no aggregate
"one route is ready" shortcut.

Use the same backend identity across providers sharing hardware. Merge
conflicting observations of one backend/served identity as unknown. Never
invent an unloaded entry for a model absent from a catalogue. `Idle` is
optional evidence for `when_idle`; set it only when the whole backend is
authoritatively idle. It cannot be inferred from the acceptable list alone.

Construct one `Health`, and share it between agent and judge:

```go
health := agent.NewHealth(agent.HealthOptions{
    Providers: cfg.Providers,
    Discovery: adapter,
    DiscoveryProviders: []string{"proxy"},
    BackendLoads: map[string]string{
        "chat-gpu": config.LoadAlways,
        "tagger-gpu": config.LoadNever,
    },
    HTTPClients: pools,
})
runner, err := agent.Open(ctx, cfg, personas, logger,
    agent.WithHealth(health), agent.WithHTTPClients(pools))
// Check err before using runner.
judge, err := agent.OpenJudge(cfg, judgePersonas, logger,
    agent.WithJudgeHealth(health), agent.WithJudgeHTTPClients(pools),
    agent.WithJudgeGuard(func(c *airut.Ctx, m agent.JudgeModel, j airut.Judgement) bool {
        return appCandidateGuard(c, m, j)
    }))
```

The application defines `adapter`, `pools`, and its guard. The snippets are
integration sketches. `DiscoveryProviders` scopes the application adapter;
omit it to own all discovery. Other providers retain native llama-swap or
unmanaged availability. Once a provider is in the adapter's scope, a missing
answer fails closed; it never falls back to unmanaged availability.
Unlisted application backend load policies default to `never`.
Native llama-swap providers use their configured `load` policy. Explicit
provider `never` restrictions cannot be overridden by the adapter.
Native backend identities are `Provider.RunningURL()`; adapters covering
the same hardware must use that identity or preserve its restriction in
`BackendLoads`. Native and application observations sharing an identity
must agree for every relevant served identity. A missing application backend or
model observation remains unknown even when native discovery reports ready or
unloaded; idle evidence must come from both sources. Application-owned routes
require an explicit backend load permission as well as the provider's permission,
including when the provider is llama-swap. Shared restrictions also apply to
native provider aliases of that backend.

The judge fits each actual candidate's input cap and runs `WithJudgeGuard`
for that candidate before final discovery. Replace an outer guard that
assumes the first judge model is always used. Keep the application's chime
mode gate outside the judge: when chime is off it should make no inference
or discovery call. Agent and judge lists remain independent even when they
share discovery, breakers and pools.

## Freshness, pooling and admission

Every selection and every dispatch check reads fresh discovery. The final
check ranks the entire role list again, including before a cold load. If
selection changes during preparation, the request is rebuilt with the new
candidate's metadata. Discovery has a three-second total deadline by
default (`DiscoveryTimeout`, at most 30 seconds), honors cancellation,
and never caches positive or negative state. An application adapter must
honor that context, bound and consume/close response bodies, and avoid
logging bodies or transport errors.

Supply long-lived `*http.Client` values in `HTTPClients`, keyed by provider.
Use the same pool for matching inference, judge, and discovery routing and
authentication; a separate sidecar may need its own pool and credentials.
`ProvidersWithOptions` also accepts these pools. Default clients own private
transports and never modify `http.DefaultTransport`. Pool reuse does not
serialize streamed responses. Applications own supplied pools and can close
their idle connections at shutdown.

The built-in OpenAI-compatible clients check discovery at their HTTP
boundary. Their otherwise-silent transient HTTP/transport retries return
to airut so the next physical attempt can choose a model and metadata
again. Custom `Options.Clients` implementations must make one physical
attempt per `Stream`, honoring the request context and limits. For clients
that read metadata internally, `Options.ClientFor` constructs a candidate
client with its effective catalog once, before requests; share HTTP pools
between those clients. A custom client must not hide inference retries or
reroute to an unobserved backend.

**Admission is best effort.** Another client can change routing or residency
between discovery and inference. A read-only sidecar, fresh checks, pooled
connections, or an application mutex cannot make this atomic. Strict
loaded-only admission requires a provider-side precondition checked
atomically with inference admission. airut does not claim to prevent every
swap or eviction in its absence. Selection logs include `best_effort=true`
and only provider/model IDs and decision kinds.

## Changes from the turn-bound chain

The former chain committed after its first output. Selection now repeats
after completed tool batches and before retries and fall-through. Failures
before output can fall through on connection errors, 5xx, 429, timeouts and
missing models. A response that already produced output fails in place;
it is not automatically replayed on another model. When no fallback is
usable, airut makes at most six transient retries with cancellable backoff;
open breakers and discovery can stop them sooner. Preparation races are
bounded to eight reselections per model step. `ErrNoModel` means no safe
candidate, and `ErrRequestTooLarge` means no attempted candidate fit.

llama-swap `/running` now requires process state `ready`; `starting` means
loading and other/missing states mean unknown. Malformed, oversized,
duplicate-key or failed inventories fail closed. The old 30-second negative
cache is gone. `NotLoadedFor` and `DefaultNotLoadedFor` remain deprecated
source-compatibility fields/constants with no effect. No deployed routing,
model capabilities, or atomic provider admission is established by this API.
