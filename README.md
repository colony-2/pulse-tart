# pulse-tart

A Go library implementing [`pulse-provider-api`](https://github.com/colony-2/pulse-provider-api) for ARM64 jobs through local Tart or Orchard. Ordinary slots run macOS; opt-in Docker slots support macOS with a private Linux companion or Linux directly. Pulse imports it and supplies complete job assignments. Configuration is a Go API; there is no provider CLI, plugin protocol, or configuration file.

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
    StateDir: "/var/lib/pulse/macos", // private, durable, absolute path
    Bases: tart.DefaultBases(),
    C2JVersion: "latest", // optional; also accepts a release tag or commit
    Slots: []tart.SlotGroup{
        {Name: "standard", Count: 2, Base: "macos-26-arm64",
         CPU: 4, MemoryMiB: 8192},
    },
}
runtime, err := tart.NewTart(cfg, tart.TartConfig{})
// Or use the same cfg with Orchard:
// runtime, err := tart.NewOrchard(cfg, tart.OrchardConfig{
//     URL: "https://orchard.example.test", Worker: "mac-worker",
//     Username: "pulse", Token: tart.Secret(controllerToken),
// })
if err != nil { return err }
defer runtime.Close()
// Pass runtime to Pulse as api.Provider. The host also drives reconciliation:
return runtime.Run(ctx)
```

Import `tart "github.com/colony-2/pulse-tart"`. Constructors validate/copy configuration and lock the journal; they create no VMs. Drive `Run(ctx)` or call `Reconcile(ctx)` periodically. `Run` returns on cancellation or a reconciliation error; the application owns its retry policy. `Slots()` reports preparation and occupancy; `Images()` reports known prepared sources.

Defaults are **4 vCPU and 8 GiB per slot**. Zero CPU/memory selects those defaults; zero count drains the group. Only one active or unresolved job uses a slot. Busy or warming slots return `no_capacity`, with no internal lease queue. Counts must fit the host's VM/resource limits; image preparation also needs a free VM position.

`DefaultBases()` supplies these digest-pinned, general-purpose images, without Xcode. It enables no slots automatically. The default SSH login is `admin`/`admin`; override credentials with a private image.

| Concrete base | Verified guest OS | Preference |
| --- | --- | --- |
| `macos-26-arm64` | Tahoe 26.6.2 | First |
| `macos-15-arm64` | Sequoia 15.7.7 | Second |

`GetCapabilities` advertises all configured bases in order, independent of current capacity. Selectors such as `macos-26*` or `macos-*arm64` match concrete names; `*` matches any sequence, other characters are literal, and matching is case-sensitive. All requested capabilities must match. Admission chooses the first configured base with a fitting available slot, then the smallest fitting slot. Add a separate slot group to enable another OS or size.

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

Publication uses Tart's `clone --overwrite` lock, which serializes replacement with cloning. Each source is published separately: failure during preparation leaves existing sources unchanged; failure during publication can leave a mix of old and new sources. Rerun the command to finish updating. Reserve one free VM position while the script builds images; it does not pause admissions for you.

The script also recognizes Orchard's worker-local aliases and replaces them alongside the canonical names. Run it **on that worker**; remote Orchard fleet updates are deferred. Requirements, naming, logs and custom-image options are in [image preparation](docs/prepared-images.md).

## Execution and recovery

The library starts `c2j run with-lease --lease-file -`, passing the opaque lease through stdin and the concrete base as `--execution-base`. c2j retrieves secrets using the lease. The provider receives no job secrets. Acceptance means Orchard accepted creation or local Tart dispatched the detached run; Pulse handles subsequent boot or deployment failures.

`Close` leaves VMs and images intact. `Drain(ctx)` disables admissions and retires slot reservations as jobs finish; continue reconciling until `Slots()` is empty. Prepared images remain reusable. For explicit removal, call `PurgeImages(ctx)` after draining and completing all jobs. It removes only sources recorded by that runtime, not other worker images or unregistered aliases.

Keep the durable journal for the owner scope's lifetime and reopen it with the same backend and owner. One runtime owns each state directory; use a distinct owner for each independent pool. Lost create responses retain reservations and are never blindly retried. Failed preparation remains inspectable through `Images()`/`Slots()` and backend events. Drain and purge known stopped sources before rebuilding a failed base. Identity mismatches are quarantined; never discard state to retry an uncertain launch.

The updater may replace published source images. Job clones retain strict ownership checks and must not be modified externally. Never run a suspended source directly. VM age alone does not authorize cleanup of a zero-timeout worker. Linux guests are available only through the opt-in Docker configuration below. Positive runtime caps, start-before constraints, scratch reservations and shared host Nix volumes remain unsupported. Guest-local Nix stores benefit from Tart/APFS disk cloning without sharing a writable store.

Orchard requires configuration-preserving suspended clones and power-state updates. Use verified HTTPS and Basic auth; `CACert` accepts a custom CA and `AllowInsecureHTTP` enables lab HTTP. Guests require SSH and passwordless sudo. Direct Tart optionally pins `Base.SSHHostKey`; otherwise it trusts the managed guest endpoint. Protect the state directory and controller access: native delivery records contain leases even though public listings and fingerprint journals do not.

## Docker capability

Docker is enabled per slot group using `Config.Docker` and
`SlotGroup.DockerProfile`. The library prepares and verifies unused capacity
before accepting a lease: one Linux VM for Linux jobs, or a fresh macOS clone
plus a private Linux Engine. It tracks both native identities through handoff,
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
Docker remains opt-in. The native integration and concurrency/fault/pressure
matrix passed on November 8, 2026; Orchard's basic integrations also passed.
Orchard-specific fault qualification and physical host power loss were not tested.

## Test and debug

Run deterministic checks with `make test vet build`. For the complete race suite, VM lifecycle and real JobDB execution:

```sh
# On an Apple Silicon Mac with Tart installed:
./scripts/test-macos.sh

# Or, against an Orchard worker:
PULSE_TEST_ORCHARD_URL=http://192.168.1.195:6120 \
PULSE_TEST_ORCHARD_WORKER=jnmac25.local \
./scripts/test-macos.sh orchard
```

Both modes test preparation, concurrent clones, reopen/replay, cleanup and actual JobDB completion: Nix execution, secret retrieval, renewal, and completed/expired lease rejection. **Tart mode also runs the standalone updater while an old clone stays running**, verifies replacement snapshot identities, and executes subsequent jobs from the new templates. That native update test still needs a real Mac validation run; the new initial-preparation flow and JobDB suite passed on Orchard.

The script prints progress and writes a Git-ignored `test-results/<timestamp>-<backend>-<pid>.tar.gz` archive containing a frozen source snapshot, full test logs and redacted diagnostics. Send that archive for debugging. Private journals and leases are excluded. Image preparation can take several minutes; native update testing builds the environment a second time. Each preparation phase allows 30 minutes within the 45-minute live-test deadline. Missing live, JobDB or required native-update assertions fail the script.

Overrides: `PULSE_TEST_BASE=macos-15-arm64`, `PULSE_TEST_C2J_VERSION=<tag-or-commit>`, `PULSE_TEST_TART_HOME`, `PULSE_TEST_SSH_PASSWORD`, or `PULSE_TEST_IMAGE` with `PULSE_TEST_OS_VERSION`. Outbound GitHub/Nix access is required. The JobDB fixture uses synthetic credentials and SSH tunnels; no existing JobDB or exposed host port is needed.

See [image lifecycle details](docs/image-lifecycle.md), [snapshot measurements](docs/suspended-templates.md), [specification](SPEC.md), and [validation records](compatibility.json).
