# pulse-tart

A Go library implementing [`pulse-provider-api`](https://github.com/colony-2/pulse-provider-api) for macOS ARM64 jobs through Orchard or local Tart. Pulse imports the library and supplies complete job assignments. There is no provider executable, plugin protocol, configuration file, or background service to install.

Both backends share `Config`: base environments, slot counts/sizes, state, and bootstrap settings. `OrchardConfig` adds controller connectivity and worker placement; `TartConfig` selects the local Tart executable/home and connection timeouts.

```go
import (
    "context"
    api "github.com/colony-2/pulse-provider-api"
    tart "github.com/colony-2/pulse-tart"
)

cfg := tart.Config{
    OwnerID: "pulse-macos",
    StateDir: "/var/lib/pulse/macos", // private, durable, absolute path
    Bases: []tart.Base{{
        Name: "macos-sequoia-15.7.7-arm64",
        Image: "ghcr.io/cirruslabs/macos-sequoia-base@sha256:eee659985b01e4df2a6c0754bd8d25e54969933c43fdcd4edef8bea337b3d354",
        OSVersion: "15.7.7", // verified inside the template
        SSHUsername: "admin",
        SSHPassword: tart.Secret(imagePassword),
        Capabilities: []string{"nix", "git", "node", "pnpm", "uv", "python"},
    }},
    Slots: []tart.SlotGroup{{
        Name: "standard", Count: 2, Base: "macos-sequoia-15.7.7-arm64",
        CPU: 4, MemoryMiB: 8192,
    }},
}
runtime, err := tart.NewOrchard(cfg, tart.OrchardConfig{
    URL: "https://orchard.example.test", Worker: "mac-worker",
    Username: "pulse", Token: tart.Secret(controllerToken),
})
// Or, on an Apple Silicon Mac with Tart installed:
// runtime, err := tart.NewTart(cfg, tart.TartConfig{})
if err != nil { return err }
defer runtime.Close()
var provider api.Provider = runtime
catalog, err := provider.GetCapabilities(context.Background())
if err != nil { return err }
_ = catalog // Complete configured bases, most preferred first.
// The host owns this loop and its error/retry policy.
return runtime.Run(context.Background())
```

Constructors validate and copy configuration, lock the journal, and create no VMs. Call `Run(ctx)` or periodically call `Reconcile(ctx)` to prepare slots and clean up completed work. `Run` returns on cancellation or the first reconciliation error. `Check` checks connectivity; `Slots` reports preparation and occupancy. `GetCapabilities`, `Submit`, and `List` support concurrent calls; discovery and listing are strictly observational.

Each slot owns an independently booted, uniquely named suspended template with a unique MAC. A job gets a fresh clone of that snapshot; only one unresolved or active job may use a slot. The original template never receives a lease. CPU/memory stay fixed during restore. Busy or warming slots return `no_capacity`; there is no internal lease queue. Counts must fit the physical host's VM and resource limits.

**Defaults: 4 vCPU and 8 GiB per slot.** Zero CPU/memory selects those defaults. A zero count drains a group. Use separate groups for larger jobs. Use concrete names such as `macos-tahoe-26.7-arm64` only for images actually running that version. Selectors match concrete names case-sensitively; each `*` anywhere matches zero or more characters, including `/` (for example, `macos-*arm64`). Other characters are literal. Aliases are no longer supported; replace old selectors such as `macos` with `macos*`. All requested capabilities must match, and configured capabilities are probed before a template becomes ready.

`GetCapabilities(ctx)` returns `BasesComplete: true` and all `Config.Bases` in configuration order, with complete capability sets and no credentials. Busy, warming, or disabled slots do not change that catalog. Results are independent copies; discovery performs no backend calls or reconciliation. For broad selectors, admission prefers the first configured base that can satisfy the request with an available slot, then the smallest fitting slot within that base. Discovery is a support hint, not a capacity reservation.

The library constructs `c2j run with-lease --lease-file -`, passing the opaque lease unchanged through stdin. It never accepts arbitrary commands or job secrets. c2j retrieves secrets through the lease. The pinned c2j receives the actual slot CPU/memory, platform, OCI image/digest, and verified capabilities; the resolved friendly base name is also available as `PULSE_BASE`. The current c2j version has no `--execution-base` flag.

Acceptance means Orchard accepted creation or local Tart's detached run was dispatched. It does not guarantee VM boot, guest delivery, or job success. Pulse handles failed deployments. Positive runtime caps, start-before constraints, scratch reservations, Linux guests, and shared host Nix volumes are unsupported. Nix and tools are preinstalled in each template; APFS clones reuse the prepared disk efficiently without sharing a writable Nix store.

`Close` releases library resources and leaves native VMs running. Reopen the same backend, owner and state directory after a host restart. Changed configuration drains affected templates; `Drain(ctx)` disables admissions and retires templates as existing jobs finish. Continue reconciling until `Slots()` is empty. Failed template builds remain for inspection; drain/re-enable a group to retry. Unknown dispatches retain reservations and are never blindly retried.

Keep the journal and its completed-launch records for the lifetime of the owner scope. One runtime owns a state directory, and one host must own each backend/owner scope. Never discard state to retry an uncertain launch. Journal version 3 rejects legacy plugin journals; drain the old provider before moving to a new owner/state directory. Identity mismatches are quarantined. Other tools must not modify provider-owned VM names or hardware.

Orchard uses REST and requires the suspended-clone behavior validated after the lab upgrade. The controller and selected worker must support suspendable VMs and power-state updates. Production connections use verified HTTPS and Basic auth; custom CA PEM is passed through `CACert`. `AllowInsecureHTTP` is an explicit lab option. Guest images require SSH and passwordless sudo. Direct Tart uses SSH password authentication; `Base.SSHHostKey` optionally pins the guest server public key. Without a pin it trusts the managed guest's SSH endpoint, as Orchard's guest delivery does.

Lease-bearing delivery data exists in Orchard's private VM startup specification or the direct backend's private native journal. The public fingerprint journal and listings contain no lease. Protect controller access and the state directory. JobRef and LaunchID are preserved in the journal and native startup metadata; Orchard worker labels remain placement selectors.

Build and run deterministic checks with `make test vet build`. For the complete test suite, including real VM and JobDB execution, use:

```sh
# On an Apple Silicon Mac with Tart installed:
./scripts/test-macos.sh

# Against an Orchard worker instead:
PULSE_TEST_ORCHARD_URL=http://192.168.1.195:6120 \
PULSE_TEST_ORCHARD_WORKER=jnmac25.local \
./scripts/test-macos.sh orchard
```

The script creates `test-results/<timestamp>-<backend>-<pid>/` and a `.tar.gz` archive beside it. Send the archive for debugging. Generated reports and archives are ignored by Git; the script remains versioned. They include host/tool versions, source fingerprints, complete Go test events and readable logs, a coverage summary, JobDB counters, and VM diagnostics captured before cleanup. Private runtime journals and exported leases stay outside the archive; diagnostic text is redacted. Successful cleanup removes the private test state; failures print the retained location.

Every run executes the whole Go suite with the race detector and a live test that:

- Prepares two independent suspended templates and runs concurrent clones.
- Checks overflow, library reopen, duplicate suppression, slot reuse and cleanup.
- Starts a disposable JobDB/Git service in a separate clone, keeping at most two VMs running.
- Acquires a real lease and submits it through the public provider API to c2j.
- Executes Nix `hello`, retrieves a synthetic secret using the lease, renews beyond the initial lease lifetime, and confirms JobDB completion.
- Rejects completed and expired leases without claiming replacement work.

Live waits print their phase and periodic progress. Template preparation allows 30 minutes, JobDB service startup 3 minutes, and each worker completion 10 minutes, within a 45-minute live-test deadline. The script fails if the live test is skipped or any required JobDB assertion is missing. No existing JobDB, manually supplied lease, production secret, or inbound host port is needed. The fixture's control, JobDB, and Git ports stay on guest loopback addresses. SSH tunnels connect the worker to the disposable service, avoiding guest-to-guest routing and host port exposure. The integration bundle includes the fixture and a test-only wrapper that waits for the tunnel, then execs the same pinned c2j binary with stdin unchanged. The normal library bundle installs neither.

Optional overrides: `PULSE_TEST_IMAGE`, `PULSE_TEST_OS_VERSION`, `PULSE_TEST_SSH_PASSWORD`, and `PULSE_TEST_TART_HOME`. Tests require outbound GitHub/Nix access and may take several minutes for first-time preparation. They never reset private state to retry an uncertain launch.

See [image preparation](docs/prepared-images.md), [snapshot behavior and measurements](docs/suspended-templates.md), [the specification](SPEC.md), and [validation records](compatibility.json).
