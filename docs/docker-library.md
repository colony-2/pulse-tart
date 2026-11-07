# Docker through the provider library

Docker is opt-in. The main library now manages provisioning, readiness, handoff
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

`Config.Docker == nil` and empty `SlotGroup.DockerProfile` preserve ordinary
macOS behavior. Profiles add `docker` to discovery for their configured bases,
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
Primary memory advertised to c2j reserves 512 MiB; an 8 GiB request does not fit
an 8 GiB VM. Increase that slot's memory accordingly.

Use an IPv4 worker address and reserve a disjoint endpoint port range per provider
owner on each worker. The
endpoint forwards only to allocation-key SSH on engine port 2222. It never
publishes Docker HTTP or container services on the physical host. Orchard must
support VM endpoints with a single-port `workerPortRange`. Local Tart installs
an allocation-owned launchd service in the worker user's domain, using
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
captures source/logs and prints a relative `test-results/...tar.gz` archive. Fresh
Nix image builds can take several minutes; each platform has a 45-minute limit.
The fixture checks a real JobDB lease, Docker work, renewal, reopen/replay,
resource listing and cleanup. The older native prototype matrix remains useful
for SDK and simultaneous-slot qualification, but does not certify this allocator.

The integrated Orchard runs passed on November 7, 2026 for both Ubuntu 24.04
and macOS 26.7.2: one claim, ten renewals, one secret resolution, reopen/replay,
Docker work and complete resource cleanup per job. Each run also passed the Go
race suite and 30 Python tests. See [recorded evidence](docker-library-results.json).
The tested client/engine versions are 29.8.1/29.1.3, Buildx 0.31.1 and the Compose
5.4.0 plugin; c2j v0.0.124 was selected for these tests. The library default still
resolves the latest c2j release during preparation.

Direct Tart's integrated path has not been run on a Mac in this environment.
Before broad production use, qualify that allocator and concurrent allocations,
real controller/worker crash boundaries, private-disk/memory/CPU pressure and
cross-allocation prune isolation. Deterministic recovery tests supplement these
live gates; they do not replace them.
