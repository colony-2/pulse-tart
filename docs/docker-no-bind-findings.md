# Docker without bind mounts: experiment findings

November 6, 2026. See the [implementation plan](docker-capability-plan.md) and
[requirements](../pulse-tart-docker-capability-requirements.md). Production support
is not enabled. These tests use the public Provider API through a prepared-VM
experimental adapter; the production slot allocator/reaper remains implementation
work. Every run used uniquely owned disposable VMs and external-controller Orchard.

## Proven execution path

A native macOS workspace can build/run Linux images and transfer files through a
private sibling engine without any shared filesystem. The real c2j v0.0.122 job
passed on Linux and macOS with a supplied current-format JobDB lease, concrete
base/platform/capability facts, one claim, ten renewals, one synthetic-secret fetch
and a successful completion. Duplicate submission after opening a new provider
process did not execute twice.

| Measurement | Linux | macOS |
| --- | --- | --- |
| c2j job including deliberate waits | 167.22 s | 166.65 s |
| Deliberate waits in that job | 135 s | 135 s |
| Cold guest + tool preparation | 28.79 s | 33.24 s |
| Cold macOS SSH readiness | — | 14.16 s |
| macOS Buildx build/load | — | 2 s |
| macOS 8 MiB binary copy round trip | — | 1 s |
| macOS Compose setup/build/health/network | — | 6 s |

Each value has one sample, except Linux has an earlier 132.23-second sample that
used v0.0.121 and lacked the second 35-second wait. Build/transfer/Compose values use integer-second
shell timers, not subsecond benchmarks. Cold preparation includes installing and
uploading tools into cached raw image clones; it is **not prepared-image or
suspended-template latency**. These are 4-vCPU/8-GiB VMs, with 7680 MiB reported to
c2j. The pair uses 8 vCPUs and 16 GiB total.

Evidence: Linux `20261106T210026Z-docker-orchard-95063`; macOS
`20261106T210854Z-docker-orchard-20073`. The latter run subsequently failed an
additional registry probe because it inherited the inaccessible admin working
directory; that does not turn the earlier c2j acceptance result into a failure
or make the overall run a pass.

The real task checked:

- Transferred local context, `.dockerignore`, executable permissions and Buildx
  load/run, including a context directory containing spaces.
- Binary archive copies in both directions, including a stopped container.
- Engine-local named volumes across containers and container tmpfs.
- Exec/logs, stdin/attach streaming, a 35-second stream and a 35-second blocking
  Docker wait. The relay's previous blanket 30-second response-header timeout
  was removed; short control-plane requests remain bounded separately.
- Compose build, service DNS/network, health dependency, dynamic loopback port,
  named volume and client-read `env_file`.

## Selected data path

Orchard worker endpoints carry end-to-end SSH from the macOS guest to a dedicated
engine service on guest port 2222. The endpoint does **not** expose Docker HTTP,
container services or the source image's password-based management SSH. The
service permits only the allocation's public key, forwarding, and no shell; its
fresh server key is pinned by the macOS relay.

This path worked while the one-shot provider processes closed and reopened.
There was no external-controller data tunnel process. This establishes independence
from those provider process lifetimes; it does not establish recovery from an
Orchard controller/worker crash or complete paired-allocation recovery.

Fixed/dynamic IPv4 publications, immediate listener connection after start,
loopback/wildcard engine publications, conflict handling, and Go Testcontainers
with archive-copied fixtures and normal Ryuk passed. The native Tart equivalent
is a detached worker TCP helper in the new Mac script. Its TCP binary transfer
and half-close logic is locally tested, but native Mac reachability/supervision
still requires a host run and production integration.

## Unsupported features are not universally rejected

Independent synthetic files on the primary demonstrated:

| Compose feature | Observed result |
| --- | --- |
| Host-path volume | `up` succeeded, but the container saw an empty engine directory; reading it as a file failed |
| Local file-backed secret | Engine rejected setup |
| Local file-backed config | Engine rejected setup |
| Named volume with bind driver options | Engine rejected setup for the missing engine-side source |

No primary-file marker was visible. Linux runtime binds can access its own local
files; they remain outside the portable job contract. There is no bind firewall,
automatic upload or filesystem synchronization. Use Dockerfile context files,
explicit copies, engine-owned volumes and client-side environment values instead.
Evidence: the fourth run's `supplement-unsupported-compose-3.json` records fresh
independent source paths for each case. Earlier supplemental attempts document
an inaccessible cwd and reused paths; the third is the qualifying observation.

## Iteration history

- `205401Z … 82474`: Linux passed. Dedicated SSH with `UsePAM no` rejected the
  locked service account. Enable PAM account handling while retaining key-only
  authentication and disabling password/keyboard-interactive access.
- `210026Z … 95063`: Linux passed the blocking wait; macOS authenticated endpoint
  passed. Probe image missing on fresh engine. Prepare explicit image/fixed-port
  fixtures rather than assuming daemon contents.
- `210600Z … 8041`: macOS API/network/Go Testcontainers passed. zsh expanded the
  unquoted JobDB query URL; quote URL arguments.
- `210854Z … 20073`: real macOS c2j job passed. Additional registry probe inherited
  an unreadable admin cwd; run task probes from an accessible directory.
- The subsequent SDK/registry-only run is recorded separately below; omitted
  c2j execution in that focused run must not be counted as new job evidence.

## Remaining production gates

Native Tart end-to-end qualification; resumed macOS templates with fresh identities;
two simultaneous macOS/Linux pairs and repeated generations; complete paired
reservation/handoff/reaper failure matrix; primary minima and memory/CPU/disk
pressure; endpoint recreation and local-helper supervision. The available Orchard
worker advertises two VM units, so it cannot host the four VMs needed for the
concurrent-pair case without an explicit capacity/configuration change.

Callbacks, UDP, arbitrary Testcontainers modules, build-secret/SSH forwarding and
unlisted Compose features remain outside the qualified v1 subset. The measured
API path does not supply these implicitly.

## SDK/registry qualification completed

`20261106T211953Z-docker-orchard-43781` passed its explicitly selected
SDK/registry-only macOS scope: authenticated registry login/push/pull/run/logout
and denied unauthenticated pull; Python Docker SDK 7.1.0 with concurrent calls;
Go Testcontainers 0.45.0 and Java Testcontainers 2.0.5 using copied fixtures and
normal Ryuk; unsupported Compose observations; TCP start/conflict checks; relay
restart with an existing service; and engine stop/restart returning an error then
recovering API connectivity. No c2j job ran in this supplementary scope; the
fourth run supplies that evidence. All owned VMs were cleaned up.

The previous supplemental `211516Z … 32319` passed registry/Python/negative Compose
but failed Java compilation due to an incorrect `Transferable` import, now fixed.
That run also had a reporting bug: skipped c2j was labeled failed and the
controller-independence description implied a job had run. Those two fields are
excluded from aggregate evidence; the revised harness leaves skipped cases unrun.

No production allocator, entire controller/worker-crash matrix, pressure guarantee
or blanket SDK compatibility is inferred from the supplementary pass.

## Recorded versions

The qualifying runs used c2j v0.0.122 (`483cb48a328ea58fe8c231b8a4e3a1c11bf39422`),
Engine 29.1.3 (API 1.52, minimum 1.44), Darwin CLI 28.5.1, Buildx 0.30.1,
Compose 2.40.3, Ubuntu 24.04 ARM64/kernel 7.0.0-42, and macOS 26.6.2 (25G83).
The controller reported its version/commit as `unknown`; worker endpoint support
was verified by actual API behavior and advertised `vm-endpoints`, not inferred
from a version string. The first run resolved v0.0.121; a new c2j release appeared
between iterations. Each archive records its resolved release and commit.
