# Docker capability evaluation — 2026-11-05

## Local Tart follow-up — 2026-11-06

The user ran `test-results/20261106T003000Z-docker-tart-6375.tar.gz` on an
M4 Max (16 cores, 128 GB), macOS 26.7.2, Tart 2.41.1. This supersedes the earlier
absence of native-host evidence below. The run completed without a run error
or cleanup error; all nine owned VMs have cleanup samples.

- Four Linux c2j jobs, SDK/Go/Java Testcontainers, isolation, and the macOS
  API/TCP relay checks passed again. Linux preparation took 35.27–36.87 seconds
  for the four job allocations.
- **Virtiofs visibility passed** across host, macOS, Linux and an inner Docker
  container at matching guest paths. This establishes basic sharing feasibility,
  not the complete filesystem or actual macOS c2j workspace contract.
- **Host-directory replacement failed:** after renaming the mounted directory
  and creating a replacement at its original path, neither guest could read
  the replacement marker. The probe labels this `stale-handle`; it does not
  distinguish retained handles from every other lookup failure. Replacing the
  directory at a fixed pathname is therefore not a validated retarget mechanism.
- macOS suspension with a share attached succeeded. Resume, suspended cloning
  and retargeting were not tested. Linux suspension was rejected explicitly:
  `You can only suspend macOS VMs`.
- Filesystem semantics still need investigation: the macOS same-guest flock
  exclusion probe failed, and case-sensitivity observations differed between
  guests. The macOS xattr probe failed because its Python lacks `os.setxattr`;
  that is a test portability defect, not evidence of unsupported filesystem
  xattrs. Linux shared-filesystem checks passed, with case-insensitive behavior.

The report remains correctly `incomplete`. A stable share-directory inode with
contents cleaned only after all old guests/readers are gone is a candidate to
test; it is not yet a proven reusable-template design. The existing Orchard
workspace-provisioning limitation is unchanged.

## Focused run at 20:10 UTC — shared-file content mismatch

`20261106T201055Z-docker-tart-86731` passed the repaired tool/resource/engine
checks and reached the shared-workspace round-trip. Admission stopped with
`shared workspace/container verification failed at finish: AssertionError`.
No c2j lease was handed off. The host/primary/engine fresh-marker checks passed;
all three owned VMs have cleanup samples and no cleanup error is recorded.

The old `finish` probe had two assertions: macOS rereading an existing file
modified by Linux, and macOS reading a new container-written file. The archive
does not identify which assertion failed or include actual/expected bytes, so
cache incoherence is a hypothesis, not yet a proven root cause. Assertions now
have distinct safe diagnostic codes. The admission guard remains strict; it does
not retry until stale data happens to match or weaken the expected contents.

A new `scripts/test-macos-docker-filesystem.sh` reproduces the relevant file
operations without compiling c2j or macOS Docker tools. Sixteen plain-guest
cases distinguish write/close from fsync and atomic replacement in both
directions, with reader-origin and writer-origin files. It records backing-host
and guest hashes, first versus delayed observations, and remount diagnostics.
Only if those pass does it install a private Linux Engine and test container
writes. It does not change host security settings or mount policy globally.

[Tart issue #1271](https://github.com/openai/tart/issues/1271) independently
reports stale/corrupted reads after external modifications in macOS virtiofs
guests, including disruptive remount workarounds. That is related evidence, not
proof that this allocation has the same defect. If the short test confirms it,
the current dual-virtiofs workspace should remain unavailable for general Docker
jobs; passing fresh-file visibility is insufficient. A private network-filesystem
variant with one authoritative workspace is then a candidate to evaluate. No
SSHFS/NFS implementation or full filesystem compatibility is claimed here.

Local verification covers the diagnostic's native-file operations, preserving
failed first reads when later reads succeed, named admission errors, cleanup,
wrapper archives/relative paths, and repository Go race tests/vet. The Mac
coherence reproduction itself still requires the next host run.

## Focused runs at 02:04 and 02:19 UTC — common preflight bug

Both `20261106T020428Z-docker-tart-19927` (cold) and
`20261106T021906Z-docker-tart-39077` (suspended) stopped during constructor
verification for the first `capabilities` call, before any c2j lease handoff.
Both passed authenticated socket/port/conflict checks and Go Testcontainers.
The suspended clone's native log explicitly records state restoration and
resume. Both reached the relay after the guarded boot-identity/shared-marker
checks. All three VMs in each run have successful cleanup samples.

The harness made `/opt/pulse-docker` mode 0700 and owned by `pulseworker`, but the
provider's `test -x /opt/pulse-docker/c2j` ran as the SSH management user `admin`.
That check cannot traverse the worker's private directory. It now runs with the
same worker identity and environment as Docker/git. The shared-workspace probe
also needs `/sbin` in PATH for `mount`; that was missing and is fixed.

Preflight failures now identify a named check (for example `c2j_executable` or
`buildx`) without exposing arbitrary guest stderr. Workspace errors retain a
safe exception/errno code. Reports retain partial boot/share observations and
mark an attempted macOS job check failed instead of leaving it `not_run`.
Shell-probe regression tests exercise worker identity, PATH, plugin failure
reporting and diagnostic redaction. Full Go race tests, vet, Darwin package
builds and Python cleanup tests pass. Rerun the cold workflow first to verify
that it progresses past admission; another live Mac pass is still required.

## Implementation follow-up — awaiting the next Mac run

The focused `scripts/test-macos-docker.sh` now implements real macOS c2j execution
with a fresh Linux engine and native macOS Docker CLI/plugins. `NewMacOS` uses the
shared Provider API, verifies the concrete primary facts and companion engine
identity, then checks primary/engine/container workspace visibility before lease
delivery. Reopened journals preserve one execution and List includes both VMs.

The directory-replacement strategy has been removed from the focused execution
path. Each prepared slot template retains a stable mounted root; jobs use fresh
children removed only after both job guests are deleted. Cold and suspended
macOS lifecycle modes are explicit; suspended mode checks preserved boot identity
and runs two real jobs through separate clones. Linux never requests suspension.
The original failing replacement/suspend probes remain in the full diagnostic
suite as research evidence, not required steps for the usable cold Linux path.

The xattr diagnostic now uses the macOS command-line API. Lock tests distinguish
lock contention from arbitrary child errors and test acquisition after release.
Case tests compare file identity to avoid drawing conclusions from cached reads.
These probe changes do not establish that virtiofs has correct locking or cache
semantics; their new results still need to be collected.

Local verification: the full test tools built successfully (including Darwin
c2j/fixture/CLI/Buildx and a checksum-verified Compose release), focused Go race
tests, workspace cleanup safety tests, and native Linux filesystem checks passed.
No new live macOS/shared-workspace success is claimed until its report arrives.

## Earlier Orchard evaluation

**Linux Docker execution is demonstrated. macOS Docker API/TCP forwarding is
demonstrated. The full macOS Docker allocation remains unproven**, chiefly because
the available Orchard cannot provision the shared workspace. Do not advertise
Docker on production macOS bases from these results.

Reproduction and code structure: [prototype guide](docker-prototype.md).
Every requested experiment has a status in the committed
[results matrix](docker-prototype-results.json). Tests not implemented or not
possible here remain `not_run`; unit tests do not replace live coverage.

## Environment and evidence

Experiments ran from this Linux controller against `jnmac25.local` through
`http://192.168.1.195:6120/v1`. Orchard reports version/commit `unknown`; its
advertised worker capacity is 16 CPUs, 128 GiB and **two total Tart VM units**.
The current host chip, host OS and deployed Tart version were not exposed by the
API and were not independently collected. Older logs/source checkouts are not
proof of the currently deployed version. The local suite records these facts
with `system_profiler`, `sw_vers` and `tart --version`.

| Component | Observed / selected |
| --- | --- |
| Linux VM | Ubuntu 24.04.5, ARM64, kernel 7.0.0-42-generic |
| Linux base reported to c2j | `ubuntu-24.04-arm64` |
| Linux image | `ghcr.io/cirruslabs/ubuntu@sha256:439ba447488fa19f9b1ed091602546b8cb32442abdb60bac4943a4d6c3dd2ffa` |
| macOS guest | Tahoe 26.6.2, build 25G83, ARM64 |
| macOS image | `ghcr.io/cirruslabs/macos-tahoe-base@sha256:4aaa751fa7538902d73c767f27a066fc87e83f183069da4e4b5022863abd4c77` |
| Docker / Buildx / Compose | 29.1.3 / 0.30.1 / 2.40.3 |
| Python Docker SDK | 7.1.0 |
| Go / Java Testcontainers | 0.45.0 / 2.0.5 |
| c2j and JobDB fixture source | v0.0.119, `472f2b4669e438cf260e7ee32c3df7a068042397` |
| Native Linux filesystem | ext4, `rw,relatime,discard,errors=remount-ro,commit=30` |

The first full-run c2j binary printed `dev` because build version flags were
omitted; the resolved source commit is recorded independently. The final builder
sets those flags. Guest OS images are digest pinned; container test images use
`alpine:3.22` and `python:3.13-alpine` tags, and apt dependencies are not frozen.
These are reproducible test commands, not a hermetic artifact build.

Raw artifacts (ignored by Git):

- `test-results/20261105T213307Z-docker-orchard-30977.tar.gz`: two Linux slots,
  two generations, real jobs, SDK/Testcontainers, direct macOS route failure,
  Linux suspension failure.
- `test-results/20261105T214415Z-docker-orchard-35282.tar.gz`: successful macOS
  authenticated tunnel/API/TCP/recovery experiments. The archive was packaged
  after completion because editing the outer shell wrapper while it was running
  interrupted its final archive step; experiment report/logs were preserved.

All test VM API records were removed; final Orchard inventory was empty.
The earlier full run retained a failed suspend record after an HTTP 412 stop
request. It was subsequently verified by UID and removed. Cleanup now handles
failed records without attempting that invalid state transition; repeated cleanup
was exercised on the final private journal. Physical worker storage reclamation
was not independently measured.

## What worked

Four real supplied JobDB leases ran c2j directly as the Linux task user. Each
claimed once, fetched its synthetic secret once, renewed six times and completed
with the expected output and exit 0. The controller helper reopened the journal
and resubmitted the same assignment without starting another worker. Actual c2j
checkouts were under `/Users/pulseworker/work/tmp/recipe-worktree-*/worktree`.
This verifies the TMPDIR integration on Linux; it does not verify a macOS shared
checkout.

The jobs built/loaded/ran images, used directory/file/space-containing/TMPDIR
binds, executable files, Compose relative mounts, dynamic ports, exec/logs/copy
and stdin streaming. Direct Python SDK and Go/Java Testcontainers with ordinary
Ryuk cleanup passed. Two private engines simultaneously used identical container
names, volume names and port 18080. Pruning one did not disrupt the other. Native
filesystem checks passed for basic rename/chmod/symlinks, case sensitivity,
same-guest process locking, user xattrs and same-user chown.

The macOS relay passed real ping, inspect, exec, logs and archive-copy operations,
loopback/wildcard dynamic TCP publications, and fixed port recovery. The probe
connected immediately after API start success; application readiness then took
0.172–0.174 seconds in two samples. A local port conflict caused start failure
and stopped the newly started container. Force-killing the relay recovered its
stale Unix socket and preexisting publication. Stopping Engine produced HTTP 502;
restarting it restored API access. Go Testcontainers with Ryuk passed from macOS.
This used the normal socket path and configured daemon-side helper socket path.

## Measured limitations

**Orchard workspace provisioning:** a VM creation request containing `hostDirs`
returned HTTP 400: “host directory sharing is disabled; restart the controller
with --insecure-allow-host-dirs to enable this unsafe feature”. No settings were
changed. The inspected Orchard schema provides host path mounts but no scoped
workspace create/delete operation. With no worker shell access available, shared
mounts, real macOS c2j execution, shared-file semantics/performance and snapshot
retargeting could not be evaluated here. This is an orchestration limitation,
not evidence that virtiofs itself fails. Do not switch to SSHFS/NFS on that basis.

**Linux suspend:** the digest above with `suspendable:true`, 4 CPUs/8 GiB and no
shares failed with `Error: You can only suspend macOS VMs` (exit 64). The image
config also lacks a machineIdentifier. Newer source support cannot be inferred
for this binary/image combination. Cold clones are the demonstrated Linux path;
there is no measured warm Linux resume result.

**Sibling networking:** the macOS guest could not route to the Linux SSH address
(`connect: no route to host`). The successful fallback forwarded an end-to-end,
allocation-key-authenticated SSH stream through Orchard and this external
controller. Neither guest received controller credentials. This proves API
transport feasibility but adds a controller data-path dependency and latency.
It does not share filesystem data. A worker-local scoped bridge is preferred.

**Capacity:** two pairs need four VM units, but this lab advertises two. We tested
two Linux allocations, then one macOS/Linux pair, not two simultaneous macOS
pairs. Each measured pair reserved 8 CPUs/16 GiB/two VM units. A proposed smaller
companion of 2 CPUs/4 GiB was not benchmarked. The Linux worker advertises
7680 MiB from an 8192 MiB VM, preserving guest overhead; macOS must not count the
companion's resources as its own.

**Remaining implementation and coverage:** no production pair allocator,
automatic abandoned-job reclamation, cancellation/lease-loss cleanup matrix,
atomic pair reservation, disk-growth reservation, live handoff fault injection,
full macOS CLI/Buildx/Compose/Java coverage, callbacks, UDP, IPv6-only networking,
CPU/memory/disk exhaustion or AMD64 tests. Admission/replay/unknown/observation
failure have focused unit tests, not the complete live failure matrix. The
macOS relay tests run as image user `admin`, not the final ordinary c2j task user.
Public image management credentials are a lab limitation. See every individual
case in the matrix; **this is a partial prototype, not completion of the full
requested validation contract**.

## Timing and resource observations

All timings below are single-lab observations, not performance targets. Images
were already cached at the worker; preparation installs tools into cold clones.

| Measurement | Samples | Observed |
| --- | ---: | --- |
| Linux create through preparation/fixture ready | 4 | 33.50–36.89 s |
| c2j lease through completion | 4 | 95.86–98.30 s, including intentional 65 s renewal wait |
| Buildx build/load | 4 | 4 s each, integer shell timer |
| Compose startup | 4 | 0–1 s, integer shell timer |
| Linux stop/API deletion | 4 | 2.04–4.07 s |
| Native 1000 × 4 KiB write/read | 4 | See per-VM filesystem JSON |
| Warm resume / shared filesystem / loaded memory | 0 | Not measured |

One prepared Linux sample used 622,272,512 bytes of guest memory (`free` used),
with 7,675,908,096 bytes available; root filesystem used 5,063,503,872 bytes.
These are guest snapshots, not physical host RSS or load measurements. The
native Git/Python fixture is small; it is not a representative dependency-install
benchmark. Do not extrapolate shared-filesystem performance from it.

## Recommended next implementation

1. Keep Linux cold-boot, private Engine support as the first candidate. Prepare
   an immutable Linux image with c2j/tools to avoid apt at admission; preserve
   independent c2j update selection. Integrate one logical LaunchID/JobRef,
   verified facts and native ownership into a durable allocator/reconciler.
2. Add a narrow Orchard workspace API: create an allocation token rooted under
   a configured worker directory; attach by token to owned VMs on that worker;
   delete only after all attachments/readers are gone. Reject traversal/symlink
   escapes and arbitrary host paths. Include recoverable operation IDs and
   inventory. General remote shell execution is unnecessary.
3. Add worker-local, allocation-scoped authenticated engine transport and
   placement/resource reservation for both guests, disk growth and relay
   overhead. Account for macOS-specific capacity separately if appropriate;
   do not simply treat a pair as one existing slot. Preserve primary minima.
4. Run native virtiofs cold-boot and exact suspend/restore experiments before
   choosing the macOS lifecycle. Cold clones with distinct allocation paths
   are the simplest candidate. Stable per-slot share paths require demonstrated
   reader teardown and zero data leakage before reuse. Do not assume replacing
   a path retargets a saved device. The local suite contains starting probes,
   but cloned restore/retarget and the full cross-guest matrix still need work.
5. Set c2j TMPDIR/work placement before its process starts and prove it through
   a supplied macOS lease. Keep delivery, keys and Docker data private. Add
   macOS CLI/plugins and finish API, callback, UDP and Testcontainers coverage.
6. Implement paired cleanup/recovery, inject every specified failure, and run
   two pairs/two generations plus pressure tests before advertising `docker`.

No change to the shared Pulse provider API is justified so far: base capability
discovery, Submit and List cover the logical allocation. The missing primitives
belong in Orchard/worker orchestration and pulse-tart's allocator.

## Reference boundaries

[Tart sharing documentation](https://tart.run/quick-start/) describes guest
virtiofs mounts. [Docker bind mounts](https://docs.docker.com/engine/storage/bind-mounts/)
resolve source paths at the daemon, so both guest paths must match.
[Apple's shared-directory API](https://developer.apple.com/documentation/virtualization/shared-directories)
allows changing existing device shares, but that does not establish a supported
Tart/Orchard retarget operation or saved-state compatibility.
[Tart 2.41.1 Linux platform source](https://github.com/openai/tart/blob/2.41.1/Sources/tart/Platform/Linux.swift)
and newer source revisions must be distinguished from the unknown deployed
binary. [Orchard's researched schema](https://github.com/cirruslabs/orchard/blob/24670c97fa5eeb9f9646cd9eb6faae63dc66a8d8/api/openapi.yaml)
explains hostDirs but is not a version attestation of this controller.
