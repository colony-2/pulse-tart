# Requirements: Docker capability in pulse-tart without bind mounts

Date: November 6, 2026. Status: requirements for implementation and targeted
prototyping in `colony-2/pulse-tart`.

This document supersedes the workspace-sharing requirements in the earlier
[virtiofs prototype request](pulse-tart-docker-capability-prototype.md).
The evolving implementation decisions and experiment gates are in the
[implementation plan](docs/docker-capability-plan.md).

**Bind mounts are outside the supported job contract.** Do not implement
virtiofs, shared host directories, NFS/SSHFS, workspace synchronization, or path
translation to supply Docker.

## Outcome and scope

A job requests its normal base and `docker` capability and receives a private,
usable Docker environment. The provider handles installation, daemon startup,
endpoint configuration, authentication, networking, accounting, and cleanup.
The job does not configure a remote engine or install Docker Desktop.

Required allocation types:

- **Linux:** c2j and Docker Engine run in the same disposable Linux VM.
- **macOS:** c2j runs in a macOS VM; a private sibling Linux VM runs Docker
  Engine. The provider manages both as one allocation.

Implement both through local Tart and Orchard. Keep the code in pulse-tart;
Pulse will depend on the library after review. Initially gate the feature behind
explicit provider configuration, preserving existing non-Docker behavior.

Use native ARM64 containers first. Docker Engine uses the Linux guest kernel;
ordinary containers do not require nested virtualization. A macOS job's Linux
companion is a sibling, not a VM launched inside the macOS guest.
[Tart Linux guest support](https://tart.run/quick-start/),
[Docker isolation model](https://docs.docker.com/engine/security/).

## Supported Docker contract

These are allocation fields, not a complete recipe:

```yaml
base: macos*
capabilities: [docker]
```

Use a Linux selector such as `ubuntu-26*arm64` for a Linux job. Report the actual
resolved base, never the selector. A Linux companion does not change a macOS
job's execution base.

| Area | Required behavior |
| --- | --- |
| Tools | Docker CLI, Buildx, and Compose v2 available to the ordinary task user; pin and test compatible versions. |
| Engine | Real Docker Engine dedicated to the allocation, with an automatically configured local client endpoint. |
| Builds | Build from the job's local context; honor `.dockerignore`; build/load and run the result; pull/push using task-provided registry credentials. |
| Containers | Create/start/stop/remove, logs, exec, attach/streaming, inspect, and ordinary user-defined networks. |
| File transfer | `docker cp` and Docker archive APIs in both directions, including stopped containers. No live filesystem sharing. |
| Storage | Engine-owned named/anonymous volumes and container tmpfs; isolated between allocations and removed with the allocation. |
| Services | Published TCP services reachable from job processes without job-specific endpoint setup; fixed and dynamically assigned ports. |
| Compose | Builds, images, service networking, health checks, published TCP ports, and engine-owned volumes within this contract. |
| SDKs | Standard Docker clients work through the provided endpoint; qualify representative Testcontainers workflows explicitly. |

Build contexts and explicit file copies use data-transfer protocols; they do
not need the Docker daemon to mount the client's filesystem.
[Build contexts](https://docs.docker.com/build/concepts/context/),
[docker cp](https://docs.docker.com/reference/cli/docker/container/cp/).

### What “no bind mounts” means

Do not support `docker run -v /job/path:/container/path`, `--mount type=bind`,
Compose host-path volumes, or named-volume driver options that disguise a host
bind as a named volume. Do not add automatic copying or synchronization to make
those constructs appear supported. The primary job keeps its normal private
workspace; its path need not exist on the engine VM.

This is a **supported-feature boundary**, not a requirement to build a Docker
API firewall. An unmodified engine may accept a bind against its own disposable
Linux filesystem. That does not expose the macOS workspace and is not a portable
job feature. Document this distinction and test the actual failure/behavior;
do not claim all such requests are rejected unless rejection is implemented.
Never attach the physical host's filesystem or Docker socket to enable them.

Provider-managed helper mounts entirely inside the engine VM—such as the
engine's own socket for a cleanup helper—may be used as implementation details.
They do not create a guest-to-guest or physical-host bind-mount promise. This
exception matters for Testcontainers helpers and must be documented in results.

Dockerfile `COPY`/`ADD` from a transferred context remains supported. BuildKit
mounts sourced from a build context or build stage are not runtime host-path
binds; test them if used by the selected build workflow. Build secrets/SSH
forwarding require their own qualification and must not be silently confused
with filesystem sharing.

Compose configurations that depend on local file-backed secrets/configs,
development/watch synchronization, or host-path volumes are not part of the
initial guarantee. Support only the tested subset; ordinary `env_file` values
read by the client are a different mechanism. Test positive and negative
examples rather than describing all Compose features as supported.
[Compose service definitions](https://docs.docker.com/reference/compose-file/services/).

No initial guarantee is made for arbitrary Testcontainers modules, UDP
publication, host networking with macOS semantics, GPUs/devices, KVM, Swarm,
cross-allocation volume persistence, or AMD64 emulation. Report these as
unsupported or separately tested, not as implied by the `docker` name.

## Implementation work

### 1. Guest types and prepared software

Add Linux guest preparation and execution to pulse-tart. Audit macOS-specific
assumptions in configuration validation, OS detection, bootstrap scripts, user
and home paths, service management, boot identity, command invocation, and
shutdown/reconciliation. Keep guest OS details internal to the implementation;
do not add OS/arch fields to the shared allocation API.

Produce a bootable, digest-pinned Linux VM image with compatible c2j and Docker
software. Pulse's existing `base2` container image cannot simply be used as a
Tart VM disk. Reuse package/preparation inputs where useful. Advertise concrete
base labels only after checking the image actually satisfies them.

Run c2j directly in the Linux allocation VM. A rootful engine inside that
allocation-exclusive VM is acceptable initially. Docker storage and volumes
must use local Linux disk, with fresh writable state for each allocation.

For macOS, install only the client/plugins and the required relay software in
the job guest; prepare the Linux engine companion separately. The companion
does not run another c2j job and does not receive the JobDB lease.

### 2. Typed configuration and admission

Add typed library configuration for enabling Docker on selected bases/slot
profiles, the engine image/software identity, companion sizing, preparation
timeouts, and provider-owned networking settings. Exact field names are left
to the implementation. Reject invalid combinations before provisioning.

The current Pulse pin (`pulse-tart` `7b32253`) permits six tool capabilities and
launches a macOS-specific worker. Audit the latest upstream implementation before
coding; reuse any newer Linux or Docker work instead of duplicating it.
[Baseline capability validation](https://github.com/colony-2/pulse-tart/blob/7b32253342afd2ece5cd24fffdffb06c9ac053ca/internal/config/config.go),
[baseline launch construction](https://github.com/colony-2/pulse-tart/blob/7b32253342afd2ece5cd24fffdffb06c9ac053ca/internal/provider/launch.go).

Advertise `docker` per base through `GetCapabilities` only when configured support
can establish this contract. Discovery remains read-only and continues to list
busy/warming bases. `Submit` checks the full requested capability set and current
capacity; it must not silently omit Docker or reinterpret the requested base.

Provision and verify the engine/endpoint before starting c2j, then pass the
concrete base and verified `--execution-capability docker`. Do not turn requested
names directly into allocation facts. Keep cold image preparation in background
maintenance where possible; do not introduce a long queue of claimed leases
waiting for Docker setup.

### 3. Docker endpoint and transport

For Linux, expose the VM's local engine to the task user. For macOS, expose an
allocation-local Unix socket and configure `DOCKER_HOST` automatically. If SDK
compatibility requires the conventional socket path as well, provide it inside
the disposable job VM. Do not require recipes to create Docker contexts.

Use authenticated, allocation-scoped transport between guests, such as an SSH
stream tunnel or mTLS. The relay must support raw streaming, exec/attach,
connection upgrades, concurrent clients, archive transfers, and long builds.
A JSON-only forwarding service is insufficient. Do not expose an unauthenticated
Docker listener on a shared network.
[Docker endpoint transports](https://docs.docker.com/engine/security/protect-access/).

Generate credentials after cloning, store them outside job artifacts, redact
them from logs, and revoke/remove them at teardown. No reusable template contains
an allocation's keys or lease. The job must not receive host or Orchard management
credentials. A shared host daemon plus different Docker contexts is not private
engine isolation.

### 4. Reachable container services

For Linux allocations, validate native published-port behavior from the worker.
For macOS, prototype provider-managed forwarding so a published port is reachable
at the same port on the job VM's loopback interface. This is the preferred v1
behavior; the recipe should not learn a companion address.

Discover fixed and dynamically assigned TCP mappings, remove stale forwards,
and reconstruct them after relay restart. Handle daemon-side loopback-only
publications using a relay inside the Linux guest; they are not necessarily
reachable at its external IP. Do not publish allocation services on the physical
host or LAN as an incidental side effect.

Resolve the race between Docker start success and forward readiness. Prototype
synchronous setup/gating or an equivalent reliable mechanism; arbitrary sleeps
in jobs are not acceptable. Report conflicts with existing macOS listeners rather
than connecting to the wrong service. Test immediate connections separately from
application-level readiness delays.

Also determine whether supported SDK workflows need container-to-job callbacks.
If needed, supply a provider-managed route/name and test it; otherwise explicitly
exclude those workflows from v1. `--network=host` on the engine means the Linux
VM's network, not macOS. UDP needs a separate design/test and is not a TCP relay
side effect.

If transparent loopback forwarding proves disproportionate or unreliable,
report a concrete alternative using native remote-engine addressing and automatic
SDK configuration. Treat that as a contract decision for review before release,
not an undisclosed change in behavior.

### 5. Paired lifecycle, resources, and Orchard

Use one LaunchID and JobRef for the macOS/Linux pair, with durable records of
both native identities. List the active resources through the existing API.
Reserve both sides before execution handoff and recover partial creation without
starting duplicate workers. Definite declines guarantee no later execution;
uncertain handoffs retain `unknown` semantics until reconciled.

Account for primary VM resources, companion resources, disk growth, and measured
overhead. Companion memory/CPU must not be reported as resources available to
macOS job processes. Keep primary requested minima intact and use an explicit
companion profile initially. Four Docker-enabled macOS slots may occupy eight
VMs; admission must reflect that footprint.

Start with same-worker placement for Orchard. With no shared filesystem,
cross-worker placement is technically a separate future option, not necessary
for v1. Prove guest connectivity/transport and ownership from a controller outside
the worker Mac; local controller access must not conceal missing Orchard support.

No `hostDirs`, host workspace provisioning, or
`--insecure-allow-host-dirs` setting is needed for this design. If Orchard lacks a
required guest-network, transport, or reservation primitive, provide the smallest
upstream requirement and reproduction. Do not use general host-command execution
as an undocumented workaround.

Prepare reusable software templates, but use fresh allocation-specific engine
state. Prototype Linux cold boot first, then suspend/resume if supported by the
selected Tart version. Existing macOS snapshot reuse must remain valid. Reconnect
agents and rotate identities after cloning; do not rely on saved live network
sessions. The earlier virtiofs/snapshot compatibility experiment is removed.

After job completion/failure, cancellation or lost lease authority, destroy both
the job resources and the companion, including all inner containers, volumes,
credentials, and writable disk state. An independent/recoverable reaper must
handle a missing primary guest and controller crashes. Preserve existing library
close/reopen semantics: a controller disconnect alone must not incorrectly kill
valid running work, and an unreachable backend is not an empty inventory.

## Prototypes required before finalizing the implementation

These are focused uncertainties. The surrounding configuration, ownership,
admission, and lifecycle requirements above are implementation work.

| Experiment | Evidence required | Decision it resolves |
| --- | --- | --- |
| Linux guest readiness | Real Docker build/run through c2j in a pinned Linux VM; cold/warm timings | Guest/bootstrap changes and minimum compatible versions |
| API relay | CLI and direct SDK calls; build progress; logs/exec streams; copy archives; concurrent/long calls; reconnect | Transport choice and whether a simple stream relay is sufficient |
| Published ports | Fixed/dynamic ports, loopback-only publication, immediate connect, conflicts, restart reconciliation | Transparent forwarding design or explicit alternative contract |
| Testcontainers | Representative Go and Java tests using copy/archive APIs, with normal cleanup helpers enabled | Qualified SDK/modules and required provider-supplied environment |
| Compose subset | Build + services + network + health check + named volume; documented unsupported file-backed features | Exact supported Compose behavior without bind mounts |
| Snapshot reuse | Two sequential allocations per slot, concurrent slots, credentials/state isolation after resume | Cold clones versus prepared/suspended engine templates |
| Orchard pair | Remote controller, correct placement, both-side reservations, guest connectivity, partial-create recovery | Minimum Orchard/library changes |
| Failure/resource pressure | Controller/guest/relay/engine failures; CPU, memory, disk pressure; competing allocations | Accounting, recovery, and cleanup design |

Testcontainers may mount its engine's own socket into a cleanup helper. Configure
that daemon-side path separately from the macOS client's endpoint. Do not disable
cleanup helpers merely to obtain a passing result. Modules that require job-file
binds remain unsupported; use copy-to-container APIs in supported fixtures.
[Testcontainers host/socket configuration](https://java.testcontainers.org/features/configuration/#customizing-docker-host-detection).

## End-to-end acceptance

Run this sequence through the public provider API on **local Tart and Orchard**,
for a Linux allocation and a macOS allocation with a companion:

1. Discover a concrete base with `docker`, without creating resources during
   discovery. Submit a real current-format JobDB lease and validate the worker's
   concrete base/capability facts.
2. Build from a private job-local context, verify `.dockerignore`, load/run the
   image, and transfer input/output files with `docker cp` or archive APIs.
   Include spaces, binary content, permissions, and a nontrivial transfer size.
3. Use Buildx and Compose within the supported subset. Create data in a named
   volume and read it from a second container in the same allocation.
4. Connect to fixed and dynamic TCP ports from the job. Exercise the qualified
   Go/Java Testcontainers workflows and at least one direct Docker SDK.
5. Repeat with two concurrent allocations using identical container/volume names
   and published ports. One allocation must not enumerate or affect the other.
   Test prune only against private disposable engines, never the host engine.
6. Attempt unsupported job-path binds and record actual behavior. Confirm that
   job/host files are not being shared implicitly and that examples/docs state
   the limitation. There is no pass criterion requiring universal API rejection.
7. Inject crashes before/after each provisioning and handoff boundary. Reopen
   the provider, avoid duplicate lease execution, and reclaim partial/orphaned
   resources. Exercise normal completion, failure, and lease-loss paths.
8. Reuse slots for new allocations and verify no old containers, named-volume
   contents, credentials, or task data survive. Measure aggregate resource use
   and verify primary allocation minima under inner-container load.

A successful `docker info` or shell demonstration alone does not satisfy these
requirements. Unit tests should cover capability/configuration changes and
failure-state transitions; real-host tests establish guest and network behavior.

## Deliverables and implementation boundary

Provide a reviewable pulse-tart branch/commit, reproducible local/Orchard test
entry points, and a Markdown findings report with machine-readable results.
Record host chip/OS, Tart/Orchard versions/commits, guest image digests/kernel,
Docker/client/plugin versions, c2j/JobDB versions, and explicit configuration.
Mark every requested experiment passed, failed, or not run. A requested live
suite must not succeed after silently skipping its live assertions.

Report sample counts and cold/warm readiness, build/transfer/Compose timings,
idle/loaded memory, disk consumption, and cleanup latency. Distinguish measured
behavior from hypotheses. Redact leases and credentials; all test resources must
be uniquely owned and cleanup must preserve unrelated infrastructure.

Document the selected production design, remaining gaps, and any necessary
Orchard changes. Keep the shared provider API dependency-free; propose additions
only if an experiment demonstrates a need. Library configuration changes should
come with a small host-integration guide for Pulse to expose them later.

This work does not deploy to production, publish images, or enable Docker in
Pulse defaults. It does not include bind-mount compatibility, workspace sharing,
automatic multi-architecture emulation, Linux-host VM providers, or a generic
Docker Desktop replacement.
