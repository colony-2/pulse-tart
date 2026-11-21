> [!WARNING]
> **Superseded by [github.com/colony-2/pulse-macvz](https://github.com/colony-2/pulse-macvz)**
> 
> This repository is no longer maintained. Please use the new repository for ongoing updates and support.

# pulse-tart

A Go library and standalone server implementing [`pulse-provider-api`](https://github.com/colony-2/pulse-provider-api) through local Tart, Orchard, or a cgo-free native VZ provider. Default macOS and Linux slots support Docker when requested by a launch. macOS Docker uses an on-demand private Linux companion; Linux starts its local daemon only for Docker requests. Pulse imports the library and supplies complete job assignments. Use the typed Go API or the Cobra `pulse-tart setup`, `upgrade`, and `server` commands. The server defaults to outbound connections through the published provider plugin SDK; it also exposes the SDK’s inbound mode. See [server CLI setup and options](docs/server-cli.md), including the current upstream macOS inbound restriction.

## Images and jobs

The provider prepares missing images automatically when you run reconciliation:

1. Clone the configured macOS image, install Nix, c2j and the worker tools, then shut it down. This becomes the shared **prepared base**.
2. Cold-boot a separate clone for each concurrency slot, with its own MAC and configured CPU/memory, then suspend it. These are the **slot templates**.
3. For each job, clone and resume an unused slot template and start its installed c2j. Delete the job clone after it shuts down. The original template never receives a lease or runs a job.

Installation happens during image preparation, not job startup. c2j is built from source inside the guest using Nix. The small setup archive transferred by the library contains scripts and build definitions, not a c2j binary or VM image; callers do not manage it.

Image names are stable and contain **no c2j version**. Existing images are reused across library restarts and upgrades. `C2JVersion` defaults to `"latest"` and is resolved only when a missing base needs installation. Changing this setting does not update existing images. There is no release manifest or installed-version metadata file; run `/opt/pulse-tart/bin/c2j version` inside a clone to inspect its version.

## Use the library

```go
cfg := tart.Config{
    OwnerID: "mac-pool",
}
runtime, err := tart.NewTart(cfg, tart.TartConfig{})
// For Orchard, also set cfg.StateDir to a private absolute directory.
// runtime, err := tart.NewOrchard(cfg, tart.OrchardConfig{
//     URL: "https://orchard.example.test", Worker: "mac-worker",
//     Username: "pulse", Token: tart.Secret(controllerToken),
//     DockerEndpoint: tart.DockerEndpoint{Address: "192.168.64.1", PortMin: 45000, PortMax: 45099},
// })
if err != nil { return err }
defer runtime.Close()
// Pass runtime to Pulse as api.Provider. The host also drives reconciliation:
return runtime.Run(ctx)
```

Import `tart "github.com/colony-2/pulse-tart"`. Constructors validate/copy configuration and lock the owner scope; they create no VMs. Drive `Run(ctx)` or call `Reconcile(ctx)` periodically. `Run` returns on cancellation or a reconciliation error; the application owns its retry policy. `Slots()` reports preparation and occupancy; `Images()` reports known prepared sources.

### Native VZ provider

`NewVZ` selects Apple's Virtualization.framework on Apple Silicon with macOS 14
or later. It uses `tmc/apple` and `purego`, builds with `CGO_ENABLED=0`, and needs
neither Tart nor host Python. Tart and Orchard remain separate options; Pulse
owns default-provider selection. VZ uses its own `~/.pulse/vz` home and `vz`
ownership scope. It does not coordinate admission with Tart.

When embedding the runner, call the hook before other work in `main`:

```go
func main() {
    tart.RunnerMain()
    // Configure and drive this Runtime as with NewTart or NewOrchard.
    runtime, err := tart.NewVZ(tart.Config{OwnerID: "pulse"}, tart.VZConfig{})
    // Handle err, then call runtime.Run(ctx).
    _, _ = runtime, err
}
```

Sign the executable with `com.apple.security.virtualization=true`, preserving
the host application's existing signing entitlements. For development, the
entitlement file is `cmd/pulse-vz/vz.entitlements`. An optional
standalone helper avoids embedding the hook in the host:

```sh
./scripts/build-vz-runner.sh "$PWD/.local/bin/pulse-vz"
```

Pass its absolute path in `VZConfig.Runner`. Production applications should use
their normal release-signing identity. `Runtime.Check` verifies the helper
protocol, virtualization support, entitlement, and inventory. This helper is
an implementation detail, not a provider configuration CLI.

Each VM runs in a detached process and survives `Runtime.Close`. The shared
local backend retains ownership binding, guest-delivery fences, and restart
recovery. VZ clones only VM data; cold clones receive fresh identities, while
suspended clones retain their MAC and machine identity. Saved state is tied to
the host OS build, device layout, and VM configuration. A missing or incompatible
snapshot fails without a cold-boot fallback; rebuild affected templates after
incompatible host or runner upgrades. Linux uses cold clones and does not suspend.
The Docker companion endpoint uses the Go runner under launchd.

The provider's live Linux lifecycle and macOS suspend/restore tests pass under
Safehouse with the required control-socket and GPU permissions. The macOS test
covers two independent snapshots, two rounds of concurrent restores, client
recovery, and corrupt-restore rejection. Public API/JobDB execution, a pinned
Ubuntu registry import and boot, and the Go endpoint's launchd lifecycle also
passed. See [the implementation plan](vz-migration-plan.md) and
[VZ setup, script map, and testing instructions](docs/vz-testing.md) for supported
entry points, reproducible checks, and limits. The ignored `.local/vz-diagnostics`
files are workstation diagnostics, not required VZ setup.

Omitted configuration selects **two macOS job VMs and four Linux job VMs**, each
with **4 vCPU / 8 GiB**. The bases are Tahoe (`macos-26-arm64`), Sequoia
(`macos-15-arm64`) and Ubuntu 24.04 (`ubuntu-24.04-arm64`). Both macOS versions
share the same two-job limit. Larger or smaller VM sizes are currently rejected.

Override any of the three job counts; omitted per-OS counts stay at 2 and 4,
and an omitted total becomes their sum:

```go
cfg.VMCounts = tart.VMCounts{
    MacOS: tart.VMCount(2), // maximum 2
    Linux: tart.VMCount(4),
    Total: tart.VMCount(3), // shared limit: at most three job VMs across both OSes
}
// tart.VMCount(0) disables an OS; Total: tart.VMCount(0) drains all job capacity.
// nil means default. ResolveConfig(cfg) returns the effective configuration.
```

These counts include warming/idle job VMs and unresolved launch reservations,
but **exclude Docker companions and image builders**. A macOS Docker job counts
as one macOS job and one total job, regardless of its Linux companion. A separate
physical limit counts every running/pending macOS VM, including builders and
foreign VMs on the worker, and never admits a third macOS instance. Suspended and
stopped source images do not count. Preparation waits for a free macOS position.

Lower limits let active jobs finish and retire unused warm capacity. With a
shared total smaller than the OS limits, a request can reclaim an idle worker
of another OS; Pulse retries while the requested pool warms. There is no lease
queue. Busy or warming slots return `no_capacity`.

Counts do not automatically scale to host resources. Six default job VMs use
24 vCPUs / 48 GiB before host overhead; two macOS Docker companions add another
8 vCPUs / 16 GiB. Choose lower counts for smaller workers.

Explicit `Bases` and `Slots` remain available for base selection. Without slots,
every configured base gets a group. The first macOS and Linux groups retain the
names `standard` and `linux`; later groups use `mac-<base>` or `linux-<base>`
(long names use a stable hash). The default Sequoia group is `mac-macos-15-arm64`.
An explicit slot group's zero `Count` drains that group; its zero CPU/memory
values resolve to 4 / 8192. Job limits apply across all groups. `C2JVersion` still
defaults to `"latest"`.

Default pools have eight configured positions: two per macOS version and four
for Linux. These are alternative per-base capacity, with **at most six job VMs
and two macOS instances** overall. Either macOS version can use both macOS
positions, or one job can run on each version.

`DefaultBases()` supplies these digest-pinned macOS images, without Xcode.
`DefaultLinuxBase()` supplies Ubuntu 24.04 ARM64. The default SSH login is
`admin`/`admin`; override credentials with a private image.

| Concrete base | Verified guest OS | Preference |
| --- | --- | --- |
| `macos-26-arm64` | Tahoe 26.6.2 | First |
| `macos-15-arm64` | Sequoia 15.7.7 | Second |

`GetCapabilities` advertises all configured bases in order, independent of current capacity. Selectors such as `macos-26*` or `macos-*arm64` match concrete names; `*` matches any sequence, other characters are literal, and matching is case-sensitive. All requested capabilities must match. Admission chooses the first configured base with an available slot. With omitted slots, adding a base automatically creates its group; the two-macOS limit applies across all versions.

The default images are ARM64. Explicit Linux bases also support `Arch: "amd64"`
on Intel Macs; macOS guests remain ARM64. Ubuntu 26.04 image builds for both
architectures are described in [Docker library setup](docs/docker-library.md).
Intel binary translation inside an ARM Linux guest is a separate feature and
is not configured or qualified here; see
[Apple's Linux translation documentation](https://developer.apple.com/documentation/virtualization/running-intel-binaries-in-linux-vms).

## Update images independently of Pulse

On the Apple Silicon worker, run this from a pulse-tart checkout as the same user, with the same `TART_HOME` (the library's `TartConfig.Home`), used by Tart or the Orchard worker:

```sh
./scripts/update-macos-images.sh \
  --owner mac-pool --base macos-26-arm64 --group standard \
  --slots 2 --cpu 4 --memory 8192

# Select a release/commit instead of the latest published c2j release:
# Add: --c2j <tag-or-commit>
# To prepare only missing images, add: --ensure
```

Match the owner, base, group, count and size to the library configuration. The script prepares a fresh base and all requested suspended slots, then replaces the published sources under their existing names. **Already-created job clones keep their old environment; clones created after replacement use the new one.** Each replacement slot gets a fresh MAC. No Pulse release, RPC, or library restart is needed.

Publication uses Tart's `clone --overwrite` lock, which serializes replacement with cloning. Each source is published separately: failure during preparation leaves existing sources unchanged; failure during publication can leave a mix of old and new sources. Use the printed `--resume` command to finish publication from retained builders, or rerun to rebuild. Reserve one free VM position while the script builds images; it does not pause admissions for you.

The script also recognizes Orchard's worker-local aliases and replaces them alongside the canonical names. Run it **on that worker**; remote Orchard fleet updates are deferred. Requirements, naming, logs and custom-image options are in [image preparation](docs/prepared-images.md).

## Execution and recovery

For an existing worker with preparation, capacity or restore problems, run
`./scripts/diagnose-tart.sh` while Pulse stays running. It saves repeated read-only
observations and a shareable archive under `test-results/`; see
[Tart diagnosis](docs/tart-diagnosis.md) for report contents and optional log inputs.

The library starts `c2j run with-lease --lease-file -`, passing the opaque lease through stdin and the concrete base as `--execution-base`. c2j retrieves secrets using the lease. The provider receives no job secrets. Acceptance means Orchard accepted creation or local Tart dispatched the detached run; Pulse handles subsequent boot or deployment failures.

`Close` leaves VMs and images intact. `Drain(ctx)` disables admissions and retires slot reservations as jobs finish; continue reconciling until `Slots()` is empty. Prepared images remain reusable. For explicit removal, call `PurgeImages(ctx)` after draining and completing all jobs. It removes only sources recorded by that runtime, not other worker images or unregistered aliases.

Direct Tart stores private properties in `$TART_HOME/vms/<name>/pulse.json` (default home: `~/.tart`), beside Tart's own files. These include the owner, launch/job identity, slot or pair association, preparation phase and native delivery details. Restart with the same Tart home and `OwnerID`; the library reconstructs its in-memory index from existing VM directories and observes Tart's current state. Deleting a VM removes its properties and reservation. An exited job restore is terminal even when Tart still reports `suspended`; reconciliation removes that clone and releases its slot. Intentionally suspended templates remain reusable.

Direct Tart needs no `StateDir` for new pools. To upgrade an existing pool, stop the old runtime and keep its original `StateDir` configured for the first new start. The library imports matching properties for VMs that still exist, leaving the old files unchanged. After migration, `StateDir` can be omitted. No new `state.json` is written. Per-VM `pulse.run.log`, `pulse.exit`, `pulse.events` and the kernel-held `pulse.run.lock` support diagnostics and restart recovery. `$TART_HOME/.pulse/<scope>/lock` excludes concurrent owners; its file contents are not an inventory.

Pulse must submit each LaunchID only once. Native launch operations are never retried on restart; only idempotent guest delivery can resume. There is no terminal launch ledger for direct Tart, and an ambiguous create with no VM does not reserve capacity indefinitely. Keep submission errors in the host logs. A partially created VM with attached properties is recoverable for cleanup; an untagged partial directory is left untouched.

Orchard still requires its private, durable `StateDir` journal because its REST API does not expose the host-side VM property files. It retains its existing duplicate/uncertain-dispatch behavior. Use a distinct owner for each independent pool. Failed preparation remains inspectable through `Images()`/`Slots()` and backend events; identity mismatches are quarantined.

The updater may replace published source images. Job clones retain strict ownership checks and must not be modified externally. Never run a suspended source directly. VM age alone does not authorize cleanup of a zero-timeout worker. Linux guests use the integrated Docker environment, configured automatically by the defaults. Positive runtime caps, start-before constraints, scratch reservations and shared host Nix volumes remain unsupported. Guest-local Nix stores benefit from Tart/APFS disk cloning without sharing a writable store.

Orchard requires configuration-preserving suspended clones and power-state updates. Use verified HTTPS and Basic auth; `CACert` accepts a custom CA and `AllowInsecureHTTP` enables lab HTTP. Guests require SSH and passwordless sudo. Direct Tart optionally pins `Base.SSHHostKey`; otherwise it trusts the managed guest endpoint. Protect the VM directories, legacy state directory and controller access: `pulse.json` contains private delivery credentials and leases. Public listings exclude them.

## Docker capability

Conventional Linux and macOS groups support Docker by default. macOS requires
a worker `DockerEndpoint` (native backends provide defaults; Orchard requires
an explicit guest-reachable worker address). Explicit groups use `Config.Docker` and
`SlotGroup.DockerProfile`. The library prepares and verifies unused capacity
before accepting a lease: Docker requests activate the Linux daemon or restore
a macOS clone with a private Linux engine. Ordinary requests do neither. It tracks both native identities through handoff,
reopen and teardown. `DockerAllocations()` exposes operational status.

The native workspace stays private. Build contexts, copies, engine-owned volumes,
TCP publications and the tested Compose/SDK subset work without shared mounts.
Runtime job-file binds and file-backed Compose secrets/configs are unsupported.

See [Docker library configuration and lifecycle](docs/docker-library.md) for the
config API, worker endpoint requirements, companion sizing, standalone image
updates and the dedicated `scripts/test-macos-docker-library.sh` test command.
For native concurrency, fault recovery, pressure and isolation qualification, run
`./scripts/test-macos-docker-qualification.sh` once with no arguments. It uses at
most two macOS VMs plus two Linux engines; run it alone. See the linked guide for
resource requirements, coverage and current validation status.
For Orchard, use `./scripts/test-orchard-docker-qualification.sh` with the
connection variables documented in the guide; the same VM limit applies.
Earlier Docker implementations passed integration and concurrency,
recovery, pressure and isolation qualification by November 8, 2026. Orchard
coverage includes API transport outage and endpoint recreation. Shared Orchard
daemon termination and physical host power loss were not tested.

## Test and debug

Run deterministic checks with `make test vet build`. For the complete race suite, VM lifecycle and real JobDB execution:

```sh
# On an Apple Silicon Mac with Tart, Go and Python 3 installed.
# Pause Pulse and other VM workloads: this suite uses both macOS VM slots.
./scripts/test-macos.sh tart

# Or, against an Orchard worker:
PULSE_TEST_ORCHARD_URL=http://192.168.1.195:6120 \
PULSE_TEST_ORCHARD_WORKER=jnmac25.local \
./scripts/test-macos.sh orchard
```

Both modes test preparation, concurrent clones, restart recovery, cleanup and actual JobDB completion: Nix execution, secret retrieval, renewal, and completed/expired lease rejection. Tart mode explicitly removes the old state directory and verifies recovery from VM properties, then injects failed restores into two real suspended clones and checks that restart releases both slots. These assertions are required for a passing report. **Tart mode also runs the standalone updater while an old clone stays running**, verifies replacement snapshot identities, and executes subsequent jobs from the new templates. The complete revised Tart race suite passed on November 15 (`20261115T230914Z-tart-51521.tar.gz`), including recovery, both replacement snapshots running concurrently, all JobDB checks and cleanup. The live test took 735.95 seconds. Dedicated Docker live suites are separate and were skipped in that run. The initial-preparation flow and JobDB suite previously passed on Orchard.

The script prints progress and writes a Git-ignored `test-results/<timestamp>-<backend>-<pid>.tar.gz` archive containing a frozen source snapshot, full test logs and redacted diagnostics. Send the archive printed on the final `Send:` line, whether the run passes or fails. Preflight and build failures also produce an archive once the reporting prerequisites are available. Private journals, VM property files and leases are excluded. Image preparation can take several minutes; native update testing builds the environment a second time. Each preparation phase allows 30 minutes within the 45-minute live-test deadline. Missing live, JobDB or required native-update assertions fail the script.

Overrides: `PULSE_TEST_BASE=macos-15-arm64`, `PULSE_TEST_C2J_VERSION=<tag-or-commit>`, `PULSE_TEST_TART_HOME` (defaults to `TART_HOME`, then `~/.tart`), `PULSE_TEST_SSH_PASSWORD`, or `PULSE_TEST_IMAGE` with `PULSE_TEST_OS_VERSION`. Outbound GitHub/Nix access is required. The JobDB fixture uses synthetic credentials and SSH tunnels; no existing JobDB or exposed host port is needed.

To isolate native restore failures without rebuilding images, pause macOS VM workloads and run `python3 scripts/probe-tart-restore.py TEMPLATE1 TEMPLATE2` with two existing suspended macOS templates that have different MAC addresses. It restores disposable clones of the first template alone, then alongside a clone of the second, twice by default (`--rounds N` changes this). It uses the same Tart home overrides and leaves the source templates unchanged. The printed archive contains native logs, hardware observations and file sizes/configuration hashes; it excludes VM disks, snapshots and Pulse property files. This probe checks native restore only, not guest readiness or JobDB execution. A passing probe does not exclude an intermittent failure.

If a pair fails to restore together, append `--compare-preparation STOPPED_BASE` to compare both restore orders, an isolated restore after failure, and three fresh snapshot pairs made from disposable clones of that base: sequential preparation, preparation with both builders running, and sequential preparation with distinct machine serials. This diagnostic mode runs once by default, keeps failures in its summary while continuing independent cases, and downloads/installs nothing. It never changes an existing source or retries a failed VM. Its cold boots are explicitly separate preparation experiments; they do not constitute a fallback for failed job restores. The November 15 comparison reproduced the failure for sequential builders sharing a Mac machine identifier; concurrent builders and sequential builders with distinct machine identifiers both restored in either order. Direct Tart preparation and the updater now assign each cold macOS builder a new identifier with `--random-serial`. This remedy passed the native probe and the complete Tart integration suite, including both replacement snapshots running concurrently and real JobDB execution. Existing affected templates require an explicit rebuild without `--ensure`; never change the identity of a saved snapshot.

See [image lifecycle details](docs/image-lifecycle.md), [snapshot measurements](docs/suspended-templates.md), [specification](SPEC.md), and [validation records](compatibility.json).

Update existing catalog-based macOS template pools with `./scripts/update-macos-images.sh --c2j main` (or omit `--c2j` for the latest release). The updater discovers their owner, base, group, hardware and existing slot indices. `./scripts/update-macos-images.sh --list` previews the targets. It excludes jobs, Linux VMs and integration-test pools; keep one macOS VM position free. Explicit `--owner` options remain available for new pools and custom recipes. See [updater options](docs/prepared-images.md).
