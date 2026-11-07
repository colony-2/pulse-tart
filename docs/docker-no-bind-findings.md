# Docker without bind mounts: experiment findings

Updated November 7, 2026. See the [implementation plan](docker-capability-plan.md) and
[requirements](../pulse-tart-docker-capability-requirements.md). Production support
is not enabled. These tests use the public Provider API through a prepared-VM
experimental adapter; the production slot allocator/reaper remains implementation
work. Every run used uniquely owned disposable VMs, through external-controller
Orchard or the native Tart matrix identified below.

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
is a detached worker TCP helper. Native Mac reachability passed the November 7
matrix below; production helper supervision/recovery remains implementation work.

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

Native Linux-primary c2j acceptance; Orchard suspended/concurrent pair qualification;
complete paired reservation/handoff/reaper failure matrix; primary minima and
memory/CPU/disk pressure; endpoint recreation and local-helper supervision;
cross-allocation enumeration/prune and complete engine-state residue checks.
The native macOS suspended/concurrent matrix passed below. The available Orchard
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

## First native suspended matrix attempt

`20261107T014911Z-docker-matrix-87479` stopped before either c2j job started.
Both slots prepared and suspended distinct macOS templates, then resumed their
first clones with matching boot IDs/MACs and no prior allocation marker/key.
Resume-to-SSH samples were 16.05 s and 12.98 s. The two templates had distinct
MACs. Both Linux engines prepared successfully.

Both slots then failed the same harness fingerprint read:
`cat /home/pulseengine/.ssh/authorized_keys: Permission denied`. Native guest
management connects as the image admin, which cannot traverse the service
account's private SSH directory. Use `sudo cat` for that public-key fingerprint;
retain the directory/key permissions. The endpoint regression fixture now models
this access restriction. The matrix also prints each child's command-failure log.

The archives record cleanup of both templates and all four allocation VMs, with
no cleanup errors. This is partial resume evidence only: guest endpoint access,
jobs, second generations and the full concurrent matrix were unqualified by that attempt.
The run used Tart 2.41.1, host macOS 26.7.2, and c2j v0.0.123.

## Native suspended/concurrent matrix passed

`20261107T022409Z-docker-matrix-35171` passed both child scopes and independent
aggregate checks. Each of two slots ran two sequential generations: four real
macOS c2j jobs completed with exit 0, one claim, ten renewals and one synthetic
secret fetch each. Both generations overlapped across slots: 165.95 s and
167.94 s of observed nonterminal execution after renewal. At most two macOS
VMs plus two Linux companions ran simultaneously.

All four macOS clones preserved their slot template's boot ID/MAC and lacked
the prior-allocation marker/key; the two template MACs differed. All four Engine
IDs and both SSH key fingerprints per allocation were distinct. Identical
container/Compose names and published ports worked concurrently. This establishes
the measured identity and reuse checks, not exhaustive engine residue or
cross-allocation prune isolation.

The native worker bridge carried authenticated SSH even though direct guest
routing was unavailable. Both rounds included builds, binary copy, Compose,
registry authentication, Python SDK, Go/Java Testcontainers with Ryuk, TCP
start/conflict checks, and relay/engine recovery probes. These exercise the
prepared-pair adapter; no production allocator fault guarantees are inferred.

| Measurement | Samples | Range |
| --- | ---: | ---: |
| macOS resumed clone to SSH | 4 | 10.12–14.91 s |
| Linux engine clone + preparation | 4 | 35.39–38.44 s |
| Real c2j job, including 135 s deliberate waits | 4 | 171.15–174.40 s |
| Native VM stop/delete cleanup | 10 | 0.19–1.01 s |

Resume-to-SSH is not complete pair readiness or handoff latency. Preparation
still installs/uploads tools into cold Linux clones. Both templates and all
eight allocation VMs were cleaned, with no reported cleanup errors. The host
was an Apple M4 Max (16 cores, 128 GB), macOS 26.7.2; Tart 2.41.1; c2j v0.0.124
(`33e2a4de4e99d3a577a38dca4b8de33d43a099ce`). This matrix selected macOS primaries;
it does not count as a native Linux-primary c2j run.

## Orchard qualification versions

The qualifying runs used c2j v0.0.122 (`483cb48a328ea58fe8c231b8a4e3a1c11bf39422`),
Engine 29.1.3 (API 1.52, minimum 1.44), Darwin CLI 28.5.1, Buildx 0.30.1,
Compose 2.40.3, Ubuntu 24.04 ARM64/kernel 7.0.0-42, and macOS 26.6.2 (25G83).
The controller reported its version/commit as `unknown`; worker endpoint support
was verified by actual API behavior and advertised `vm-endpoints`, not inferred
from a version string. The first run resolved v0.0.121; a new c2j release appeared
between iterations. Each archive records its resolved release and commit.
