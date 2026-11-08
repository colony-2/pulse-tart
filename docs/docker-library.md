# Docker through the provider library

Docker is included in conventional Linux groups and opt-in for macOS. The main library manages provisioning, readiness, handoff
and cleanup; applications do not import `experimental/docker` or provision VMs.
The earlier prototype's successful matrix establishes the architecture. Production
library integration has its own tests and remaining live qualification gates.

## Configuration

```go
engine := tart.DefaultDockerEngine()
cfg := tart.Config{
    OwnerID: "mac-pool", StateDir: "/var/lib/pulse/mac-pool",
    Bases: []tart.Base{tart.DefaultBases()[0], tart.DefaultLinuxBase()},
    Docker: &tart.DockerConfig{Profiles: []tart.DockerEngineProfile{engine}},
    Slots: []tart.SlotGroup{
        {Name: "mac", Base: "macos-26-arm64", Count: 1,
         CPU: 4, MemoryMiB: 8192, DockerProfile: "standard"},
        // Enable Linux capacity by adding another group, sized for the host:
        // {Name: "linux", Base: "ubuntu-24.04-arm64", Count: 1,
        //  CPU: 4, MemoryMiB: 8192, DockerProfile: "standard"},
    },
}
endpoint := tart.DockerEndpoint{
    Address: "192.168.64.1", // native Tart host address reachable from guests
    PortMin: 45100, PortMax: 45120,
}
runtime, err := tart.NewTart(cfg, tart.TartConfig{DockerEndpoint: endpoint})
// Orchard uses the same cfg. Set DockerEndpoint.Address to its worker's IP:
// runtime, err := tart.NewOrchard(cfg, tart.OrchardConfig{
//     URL: "https://orchard.example", Worker: "mac-worker",
//     Username: "pulse", Token: token, DockerEndpoint: endpoint,
// })
```

With omitted slots, each macOS base gets two template positions sharing a
two-job limit, and the default Linux base gets four positions. Every VM uses
4 vCPU / 8 GiB. Linux gets the standard Docker profile automatically. Set
`Docker: &tart.DockerConfig{}` to enable that profile for macOS too; this requires
a backend `DockerEndpoint`. Linux-only Docker needs no worker endpoint.
Explicit empty `SlotGroup.DockerProfile` preserves ordinary macOS behavior.
Profiles add `docker` to discovery for their configured bases,
including busy/warming/drained groups. Ordinary groups retain their original
execution facts. Linux currently requires an Ubuntu 24.04 ARM64 base and a Docker
profile. No shared Provider API fields or dependencies change.

The default companion uses four CPUs, 8 GiB RAM, and a 32 GiB private ext4 Docker
filesystem, with Engine 29.1.3 checked before admission. CPU/memory reservations
are submitted separately for both VMs. The bounded Docker filesystem is not a
host disk-space reservation: provision enough worker storage for all configured
slots, source images, Nix stores and guest OS overhead. Count two running VMs,
eight CPUs and 16 GiB per macOS Docker allocation. A Linux allocation uses only
its primary VM. Companion resources never inflate primary execution facts.
On Orchard, macOS VMs request one `org.cirruslabs.tart-vms` slot and Linux VMs
request zero; both still reserve their full CPU and memory. This allows two
macOS Docker allocations on a worker advertising two macOS slots. Existing
allocations keep their recorded reservations until reclaimed.
Primary memory advertised to c2j reserves 512 MiB; the fixed 8 GiB VM supports
job memory requests up to 7.5 GiB. Larger VM envelopes are currently unsupported.

`Config.VMCounts` limits job VMs across all groups: macOS defaults to two, Linux
to four, and total to their sum. Nil selects a default; `tart.VMCount(0)` disables
that category. Companions and builders do not consume these job limits. They
still consume real host CPU, memory and storage. All macOS instances, including
builders, are subject to the separate two-instance physical ceiling. Idle warm
job VMs reserve capacity; a request for another pool can retire idle capacity
when the shared total is full. Active jobs and uncertain dispatches retain their
reservations until cleanup.

Use an IPv4 worker address and reserve a disjoint endpoint port range per provider
owner on each worker. The
endpoint forwards only to allocation-key SSH on engine port 2222. It never
publishes Docker HTTP or container services on the physical host. Orchard must
support VM endpoints with a single-port `workerPortRange`. Local Tart installs
an allocation-owned launchd service with `LimitLoadToSessionType=Background`
in the worker user's domain, using
`/usr/bin/python3` (override `TartConfig.PythonBinary` if needed). Its script,
plist and logs live under the private state directory. The service and VMs
survive `Runtime.Close`; run reconciliation again to recover and clean them up.

## Preparation and jobs

Run `Run` or `Reconcile` in the host application. Image installation and warm
allocation provisioning happen before admission. A warming or exhausted group
returns `no_capacity`; no lease waits in an installation queue. `DockerAllocations()`
reports primary/companion identities and phases without credentials or leases.

macOS Docker sources have a separate `-docker` prepared base and a template per
slot/profile. Templates contain software, never allocation keys or a lease. Each
allocation resumes a new primary clone and cold-boots a private Linux companion.
Linux workers cold-boot a prepared Nix/c2j base. Linux suspension is not required.
The selected c2j release and its Go dependency hash resolve during preparation;
no c2j release is compiled into the library. Docker client/plugins come from the
pinned Nixpkgs input. Engine setup verifies the configured server version.

For standalone macOS source updates, match the slot profile:

```sh
./scripts/update-macos-images.sh --owner mac-pool \
  --base macos-26-arm64 --group mac --slots 1 \
  --cpu 4 --memory 8192 --docker-profile standard
```

Already-created clones, including unused warm capacity, keep their existing
software. Replacement sources apply when the next clone is created. To replace
unused warm capacity immediately, drain that group, reconcile its cleanup, then
restore its count. Active jobs are preserved by draining.

Before handoff, the provider checks the primary OS, resources and tools and
verifies that its Docker endpoint reaches its own engine identity. Only the
primary receives the opaque lease on stdin. c2j receives the selected base and
verified allocation capabilities. The worker uses a native private workspace;
`DOCKER_HOST` and the Testcontainers host/socket settings are supplied automatically.

## Ownership and recovery

The private journal records both native specs/UIDs, the slot, fixed endpoint port
and provisioning/handoff phases before mutations. A lost create response is
observed by exact identity, never retried blindly. Unknown create absence keeps
its reservation; known partial resources can still be reclaimed. A handoff error
returns `unknown`. Reopen observes the exact guest receipt rather than sending
another lease. Duplicate LaunchIDs retain their original result/fingerprint.

`List` returns both owned native resources with the same LaunchID and JobRef,
including stopped resources pending cleanup. Capacity remains reserved until
both deletions are confirmed and the native endpoint helper is removed. Backend
observation failures never mean free capacity. Identity mismatches quarantine
resources rather than deleting replacements. `Drain` reclaims unused warm pairs
and lets active jobs finish. VM age does not terminate zero-timeout jobs.

The host must supervise `Run`: it returns reconciliation errors so the application
can log/retry. Endpoint supervision does not replace allocation reconciliation.
No RPC to Pulse coordinates updates or teardown.

## Supported contract and validation

Build contexts, Buildx, explicit copies/archive APIs, named/anonymous volumes,
tmpfs, TCP publications and the tested Compose/SDK subset use private engine
storage. Same-port IPv4 loopback forwarding is gated on container start; failures
are returned without replaying Docker mutations. Runtime workspace binds,
file-backed Compose configs/secrets, UDP, callbacks and arbitrary Testcontainers
modules remain outside the supported contract. There is no bind-mount firewall.

Run the full deterministic suite plus real Linux and macOS Docker library jobs:

```sh
./scripts/test-macos-docker-library.sh
# Or: ./scripts/test-macos-docker-library.sh orchard
# Focused platform: ./scripts/test-macos-docker-library.sh tart darwin
```

Orchard mode requires `PULSE_TEST_ORCHARD_URL`, `PULSE_TEST_ORCHARD_WORKER` and
`PULSE_TEST_ENGINE_WORKER_ADDRESS`. The script runs platforms sequentially,
captures source/logs and prints a relative `test-results/...tar.gz` archive. Native
macOS testing first checks launchd bootstrap, forwarding, Close/reopen, same-port
restart and removal without creating any VMs. A failed preflight stops the run;
its logs and endpoint diagnostics are included in the archive. Fresh
Nix image builds can take several minutes; each platform has a 45-minute limit.
The fixture checks a real JobDB lease, Docker work, renewal, reopen/replay,
resource listing and cleanup. The older native prototype matrix remains useful
for SDK and simultaneous-slot qualification, but does not certify this allocator.

The integrated Orchard runs passed on November 7, 2026 for both Ubuntu 24.04
and macOS 26.6.2: one claim, ten renewals, one secret resolution, reopen/replay,
Docker work and complete resource cleanup per job. Each run also passed the Go
race suite and 30 Python tests. See [recorded evidence](docker-library-results.json).
The tested client/engine versions are 29.8.1/29.1.3, Buildx 0.31.1 and the Compose
5.4.0 plugin; c2j v0.0.124 was selected for these tests. The library default still
resolves the latest c2j release during preparation.

Direct Tart's Linux integration passed in native run
`20261107T042440Z-docker-library-tart-85305`. The macOS integration passed on
November 8 in `20261108T014458Z-docker-library-tart-84863` after the launchd
Background session fix: one claim, ten renewals, one secret resolution,
reopen/replay, Docker work and paired cleanup. The live case took 471 seconds;
the native endpoint preflight took 2.32 seconds. The run also passed the Go race
suite and all 31 Python tests. The unrelated `TestLive` skip is expected in this
driver; `TestLiveDockerLibrary` is the required live test.

The native qualification has one command, with no arguments:

```sh
./scripts/test-macos-docker-qualification.sh
```

This runs the regression suite and three generations of two production macOS
allocations. Each has its own Linux engine. It checks overlapping real c2j jobs,
identical container/volume names and TCP ports, cross-allocation prune isolation,
controller SIGKILL and replay, guest relay/worker endpoint SIGKILL, Docker daemon
restart, primary/engine VM loss, paired cleanup, and fresh jobs after faults.
All six allocations must have distinct engine IDs and transport keys. Four jobs
must complete with one claim each; the two jobs used for VM-loss injection are
expected to be interrupted.

Pressure stays inside disposable engines: four CPU workers with a 3.5-CPU quota
and roughly 4 GiB of touched memory, a separate 64 MiB container OOM test, and
ENOSPC on a verified private 8 GiB loop filesystem. The other allocation must
remain reachable throughout. These are bounded isolation checks, not physical
host exhaustion tests. The CPU/memory controls use Docker's documented
[container resource limits](https://docs.docker.com/engine/containers/resource_constraints/).

Allow approximately 30–60 minutes plus downloads; the test limit is 100 minutes,
including cleanup allowance. Peak allocation size is four VMs / 16 vCPUs /
32 GiB RAM, plus host overhead. No more than two macOS VMs may run, including
base/template builders; the test guards native create dispatch as well as checking
inventory. Stop other running VMs first and run this suite alone. It requires at
least 40 GiB free in the Tart directory and uses worker ports 45200–45201.

The script archives frozen sources, progress, per-stage evidence, real JobDB
results and redacted diagnostics under `test-results/`, and prints relative
paths. A missing stage, reused identity, duplicate claim, failed cleanup, skipped
live test or failed regression makes the report incomplete. Failed runs retain
the private recovery state path printed in the log; credentials are excluded
from the archive. `TestLive` and `TestLiveDockerLibrary` skips are expected in
this driver: the required live test is `TestLiveDockerQualification`.

The native qualification passed on November 8, 2026 in
`20261108T023758Z-docker-qualification-52820`, testing revision `de6211b`.
The live test took 1,046.60 seconds (about 17.5 minutes). All twelve stages passed,
including four completed JobDB jobs, six distinct allocation identities,
distinct concurrent MAC addresses, and a peak of two running macOS VMs.
The Go race suite and all 42 Python tests also passed. The frozen source hashes
and qualification report were independently verified against the archived
evidence. No further run is required for this planned native matrix.

This result covers the listed native worker scenarios; physical host power loss
was not tested. The basic library script remains useful
for routine integration: no arguments selects Linux and macOS; `tart darwin`
selects macOS only. There is no need to run both forms to qualify one revision.

Run the corresponding Orchard matrix from any machine with Go, Python and
network access to the controller:

```sh
PULSE_TEST_ORCHARD_URL=http://192.168.1.195:6120 \
PULSE_TEST_ORCHARD_WORKER=jnmac25.local \
PULSE_TEST_ENGINE_WORKER_ADDRESS=192.168.64.1 \
./scripts/test-orchard-docker-qualification.sh
```

The endpoint address must be reachable from the guests. Authenticated controllers
can use `PULSE_TEST_ORCHARD_USERNAME` and `PULSE_TEST_ORCHARD_TOKEN`. Run on an idle
worker with at least 16 CPU cores, 32 GiB memory plus host overhead, and two macOS
slots. The same two-macOS-VM limit, private 8 GiB engine disks and three generations
apply. The host-local launchd preflight is specific to the native script.

Orchard requires thirteen stages. It replaces the native host endpoint SIGKILL
with withdrawal of an owned Orchard endpoint, observed loss of connectivity,
and reconciliation restoring the original port. It also injects a 30-second API
transport outage into the provider: an independent observer must see guest jobs
continue, failed inventory calls must leave the journal unchanged, and recovery
must accept duplicate submissions without another claim. The separate provider
controller process is killed just as in the native matrix. These tests do not
terminate the shared Orchard controller or worker daemons or power off the host.

The Orchard qualification passed on November 8, 2026 in
`20261108T031434Z-orchard-docker-qualification-17477`. The live test took
1,090.22 seconds (about 18 minutes). All thirteen stages passed, with four
completed JobDB jobs, six distinct allocation identities, distinct concurrent
MAC addresses, and a peak of two running macOS VMs. Both macOS/Linux pairs ran
concurrently with the corrected Linux slot accounting. The Go race suite and
42 Python tests passed in the frozen run; the final regression suite passed
44 Python tests, Go race tests, vet and Darwin ARM64 compilation.

The archive's 144 source hashes and report were independently verified. Orchard
reported no remaining VMs after cleanup. No further run is required for this
planned Orchard matrix; the shared daemon and physical power-loss scenarios
above remain outside its scope. Exact evidence paths and checks are recorded in
[`docker-library-results.json`](docker-library-results.json).
