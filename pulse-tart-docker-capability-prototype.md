# Request: prototype and validate Docker-capable Tart/Orchard allocations

Date: November 5, 2026. Status: implementation and experiment request for
`colony-2/pulse-tart`; not a request to enable Docker in production defaults.

## Objective

Build and test a prototype that can supply `capabilities: [docker]` through the
existing Pulse provider API, with no job-specific Docker setup:

1. A **Linux allocation** runs c2j and a private Docker Engine in one Linux VM.
2. A **macOS allocation** runs c2j in a macOS VM and uses a private sibling Linux
   VM for Docker. Both guests mount the same provider-owned host workspace using
   **virtiofs**, at the same absolute path.

For the macOS case, same-host virtiofs is the preferred experiment. Do not begin
with SSHFS, NFS, or file synchronization. Investigate those only if a measured
virtiofs limitation prevents the desired behavior, and explain the tradeoff.

Deliver working prototype code, reproducible real-machine tests, measured
results, and a proposed production implementation plan. A negative result is
useful if it identifies the exact failing configuration and viable alternatives.
Do not substitute mocked tests or `docker info` for end-to-end validation.

The design rationale and alternatives are in the
[Docker capability proposal](../docker-capability-proposal.md). This request
supersedes its earlier preference for guest-to-guest workspace sharing.

## Desired user experience

The job requests its execution base and Docker capability. These are allocation
fields, not a complete recipe:

```yaml
base: macos*
capabilities: [docker]
```

Without manually supplying endpoints, sockets, credentials, mount mappings, or
forwarded ports, the job can:

- Run `docker`, `docker buildx`, and `docker compose` as its ordinary task user.
- Build an image from its current workspace and run it.
- Bind-mount `$PWD` and a provider-managed temporary directory, with writes
  visible in both directions.
- Start services and reach their published TCP ports, including dynamically
  assigned ports, from the job VM.
- Use Docker SDKs and representative Testcontainers workflows with normal
  cleanup helpers enabled.

“No outside configuration” means the provider owns setup after infrastructure
installation. Document any required host/Orchard configuration explicitly; do
not conceal it in the test harness. A lab-only controller option can establish
feasibility, but is not evidence that the production setup is automatic.

## Architectural constraints

Keep the prototype in `pulse-tart`, using the shared provider API. Pulse will
consume the resulting library after review. Do not copy the implementation into
Pulse or add a plugin framework dependency.

The existing API should be sufficient: per-base capability discovery, allocation
capability requirements, Submit, and List. Keep it dependency-free. If an
experiment exposes a genuine API gap, document a minimal proposed change instead
of silently extending the contract.

Use one engine per allocation. Do not expose the physical host's Docker socket
or share a daemon between jobs. Containers and named volumes belong to the
allocation; an ordinary Docker prune inside that private engine must not affect
another allocation or infrastructure.

Tart already supports Linux guests. Docker Engine in a Linux VM does not require
nested virtualization. Do not make M3+ hardware or nested-virtualization flags a
prerequisite for ordinary Docker containers. The host must still support the
chosen Tart/guest versions. Running an accelerated Linux VM inside the macOS
guest is not the proposed topology.
[Tart Linux guests](https://tart.run/quick-start/),
[Docker kernel isolation](https://docs.docker.com/engine/security/).

Use native ARM64 containers initially. Record AMD64 emulation separately if
tested; do not infer it from Docker availability. A macOS allocation still
reports its concrete macOS base to c2j, regardless of its Linux companion.

## Experiments to implement

### 1. Establish a Linux Docker allocation

Prepare a digest-pinned, bootable Linux VM image with the worker and Docker
Engine, CLI, Buildx, and Compose. A container image such as Pulse's `base2` is
not itself a bootable Tart VM image; reuse preparation inputs where appropriate.

Run c2j directly in the VM and provide an allocation-private engine. Initially,
rootful Engine inside the disposable VM is acceptable for compatibility. Keep
Docker's image store and named volumes on the VM's local Linux filesystem.

Verify actual guest OS, architecture, tools, engine availability, and the requested
capabilities. Execute a real supplied JobDB lease, report the concrete base, and
exercise a Docker build and container launch through c2j.

Start with cold boot correctness. Then test the selected Tart release's Linux
suspend/resume behavior, including machine identity and device requirements.
Report supported version combinations; do not assume that current source support
exists in every deployed Tart binary.

### 2. Share one host workspace between macOS and Linux

Create an allocation-owned directory on the physical Mac. Mount it into both
guests with virtiofs at an identical absolute path, for example:

```text
Physical Mac: <private-provider-root>/<allocation>/workspace
                   ├── macOS VM: /Users/pulseworker/work
                   └── Linux VM: /Users/pulseworker/work
```

The physical-host path need not equal the guest path. The two **guest paths must
match**, because Docker interprets bind sources in the Linux daemon's filesystem.
Do not rewrite arbitrary Docker API bind paths or intercept only CLI commands;
SDKs and Compose must see the same behavior.
[Docker bind-mount semantics](https://docs.docker.com/engine/storage/bind-mounts/).

Make the actual c2j checkout/work directory reside there. Identify and test the
worker integration needed to place its task temporary files in a shared subtree.
Do not claim success from manually running a shell in the shared directory while
real jobs continue using the macOS guest's private disk.

Keep guest credentials, JobDB lease delivery, engine data, and controller state
outside the shared workspace. Do not attach the same writable block disk to both
guests as a substitute for a shared filesystem.

Test:

- Host, macOS, and Linux can observe writes; an inner Docker container reads and
  writes the same data through ordinary `-v "$PWD:/work"` and `--mount` syntax.
- Directories, single-file mounts, paths containing spaces, relative Compose
  mounts, temporary files, executable files, and nested directory trees.
- UID/GID differences, chmod/chown, rename, symlinks, file locking, concurrent
  writers, case sensitivity, and relevant xattrs.
- Cross-guest filesystem notifications separately from file visibility. Record
  whether watch-based workflows require polling; do not equate the two.
- A Git checkout and a representative dependency installation/build workload.
- Symlinks and path traversal cannot expose another allocation's workspace or
  infrastructure files.

Compare these tests against each guest's native disk. Record filesystem types,
mount options, observed ownership, and unsupported operations.

### 3. Resolve virtiofs versus suspended-template lifecycle

This is a primary feasibility question, not an optimization to postpone.
Existing pulse-tart execution depends on reusable prepared templates.

Determine experimentally:

1. Can the exact macOS and Linux guest/device configurations suspend and resume
   with a directory share attached?
2. Can a cloned suspended VM use a different allocation's share, or is the
   share/device configuration effectively fixed by the saved state?
3. Can shares be attached or retargeted after resume using supported interfaces?
4. Does replacing a host directory at the same pathname retain stale directory
   handles, cached content, or connections in a resumed guest?

Compare feasible lifecycle choices: cold-boot clones with per-allocation shares,
preconfigured per-slot share paths with verified cleanup, and any supported
post-resume attachment. A per-slot path is acceptable only if old guests and
readers are gone before reuse and sequential jobs cannot observe prior data.

Demonstrate two independent slots concurrently and at least two sequential jobs
per slot. Include controller restart and host-directory replacement tests. Report
whether the choice changes current warm-start behavior and measure the cost.

### 4. Provide a usable Docker API and service network

Install the CLI/plugins in the macOS worker and expose an allocation-local Unix
socket, with environment/context configuration managed by the provider. Bridge
it to the Linux daemon over authenticated, allocation-scoped transport. Sharing
a socket pathname through virtiofs does not connect sockets across two kernels.

Exercise API ping/version/info, pull/build, logs, exec, attach/streaming, copy,
concurrent clients, Buildx build/load, and Compose up/down. At least one test must
use a Docker SDK directly, without a shell wrapper. Test an application that
expects the conventional socket path as well as one that honors `DOCKER_HOST`;
state which behavior the prototype supplies.

For published ports, implement and test both fixed and dynamic TCP mappings.
Target the desired ordinary workflow of connecting to `localhost:<published>`
inside the macOS job VM. A connection only to the Linux VM's external address
is useful intermediate evidence, not full transparency.

Include:

- Loopback-only and wildcard publications on the daemon side.
- Immediate connection after a successful start, with bounded retries only for
  the application's own readiness. Do not hide relay-registration races with
  arbitrary sleeps.
- Conflict detection when a macOS process already occupies the target port.
- Restart/reconnect of the relay while containers remain active, including
  discovery of already published ports.
- Container-to-job callbacks and a defined route/name for reaching the macOS VM.
- UDP behavior as an explicitly separate result; do not imply it from TCP tests.

If asynchronous forwarding races with Docker API success, prototype gating
relevant API completion or another mechanism that closes the gap. Document the
complexity and remaining semantic differences. `--network=host` on the engine
refers to Linux, not to macOS; do not advertise otherwise.

Run Go and Java Testcontainers examples with normal resource-reaper behavior.
Configure host discovery and daemon-side socket mounts internally where needed;
the socket path used by a Linux helper is not necessarily the macOS client path.
[Testcontainers configuration](https://java.testcontainers.org/features/configuration/).

### 5. Exercise the same design through Orchard

Both guests and the workspace must reside on the selected Orchard worker. Test
from a controller outside that Mac, so local filesystem access cannot accidentally
stand in for missing remote orchestration.

At the researched Orchard revision, `hostDirs` exists but requires
`--insecure-allow-host-dirs`; the Pulse-pinned library's backend model does not
expose it. Recheck the actual versions used for the prototype. Identify how the
provider creates, owns, mounts, and deletes a workspace on a remote worker—not
just how it passes an already-existing path to VM creation.
[Orchard schema inspected during research](https://github.com/cirruslabs/orchard/blob/24670c97fa5eeb9f9646cd9eb6faae63dc66a8d8/api/openapi.yaml),
[pulse-tart backend model inspected during research](https://github.com/colony-2/pulse-tart/blob/7b32253342afd2ece5cd24fffdffb06c9ac053ca/internal/backend/model.go).

Document the minimum Orchard changes if scoped workspace provisioning is absent.
Prefer provider-owned allocation directories and constrained mount requests to
arbitrary host paths or general host-command execution. An explicitly configured
lab using the existing flag may validate mounts; clearly separate that from the
recommended production API. Do not silently change an existing controller's
security settings.

Record worker placement, VM/resource reservations, and how partial pair creation
is recovered. Do not assume two separate VM-create requests are atomic.

### 6. Verify capability admission, ownership, and failure behavior

Use one logical LaunchID for the allocation, preserving JobRef on both native
VMs and in discovery/listing. The companion does not need the JobDB lease or
Orchard management credentials. Generate engine/relay credentials per allocation,
not in a reusable snapshot. Do not expose the engine to other workers through an
unauthenticated shared network.

Advertise `docker` in per-base `GetCapabilities` only for the enabled prototype
configuration that can supply it. Discovery stays read-only; busy/warming support
does not disappear. Before c2j starts, verify this allocation's engine, mounts,
and required network behavior, then report the verified capability. Requested
names alone are not allocation facts.

Reserve both VMs, disk growth, and relay overhead. Preserve primary-worker CPU
and memory minima; do not add the companion's resources to the macOS allocation
facts as though they are available to macOS processes. Use an explicit companion
size for the experiment and report total footprint. Four macOS slots plus four
companions must not retain the accounting of a four-VM pool.

Inject failures after creating either VM, while preparing mounts, after setting
credentials, during lease delivery, and after c2j has started. Also kill the
controller, engine, and relay independently. Verify:

- Definite declines cannot later execute the job.
- Ambiguous handoffs remain `unknown` and do not trigger duplicate fallback.
- Restart recovers ownership without running the supplied lease twice.
- Completion, failure, cancellation/lease loss, and abandoned attempts eventually
  reclaim companions, relays, credentials, workspaces, and mutable disk state.
- An unreachable backend does not get mistaken for an empty inventory or free
  capacity.

Measure CPU saturation, memory pressure, and disk exhaustion with inner
containers. Use two simultaneous allocations with identical container names,
volume names, and published ports to demonstrate isolation. Destructive Docker
cleanup tests must target only their private engines, never the host daemon.

## Deliverables and evidence

Provide:

1. A reviewable prototype branch/commit in pulse-tart, with an explicit
   experimental configuration and no change to normal defaults.
2. A single documented entry point for each real-host suite: local Tart and
   Orchard. Provision only uniquely named, owned disposable resources; cleanup
   must be repeatable and preserve unrelated VMs, images, and workspaces.
3. A machine-readable report plus a concise Markdown findings document. Record
   host chip/OS, Tart/Orchard versions and commits, guest image digests/OS/kernel,
   Docker/client/plugin versions, filesystem/mount configuration, and c2j/JobDB
   versions. Redact credentials, leases, and sensitive job data.
4. An explicit result for every requested experiment: passed, failed, or not
   run, with commands, sanitized logs, and reasons. A suite requesting live
   coverage must not report success after skipping that coverage.
5. Cold and warm preparation/ready times, shared-filesystem workload timings,
   Docker build/Compose startup, idle and loaded memory, disk use, and cleanup
   latency. Include sample counts and native-disk baselines; do not invent
   performance targets before measuring.
6. A recommended production design and requirements for pulse-tart, Orchard,
   images, and any c2j workspace integration. Separate proven behavior from
   hypotheses and remaining work; propose API changes only where necessary.

The prototype is ready for review when it establishes which combinations work,
which fail, and why. Production `docker` support is ready only after the selected
design passes the full job, filesystem, API, network, isolation, accounting, and
recovery contract. If same-host virtiofs fails a necessary property, include the
smallest reproduction and the cost of the best alternative.

## Scope boundaries

This request authorizes prototype implementation and tests in pulse-tart, not
production deployment, publishing images, or changing Pulse defaults. Preserve
the existing macOS provider behavior outside the experimental configuration.
Linux-native host providers, shared host Docker access, rootless DinD/Sysbox,
general Docker Desktop replacement, GPU/KVM passthrough, and automatic
multi-architecture emulation are outside the initial prototype.
