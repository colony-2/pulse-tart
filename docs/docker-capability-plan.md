# Docker capability implementation plan

Status: design selected; native suspended/concurrent prototype passed November 7, 2026;
production qualification still has gates. Implements the
[no-bind requirements](../pulse-tart-docker-capability-requirements.md).
Production defaults remain unchanged. Qualification results below distinguish
live evidence, implementation decisions, and release gates.

## Selected contract

Each allocation owns its Docker Engine and mutable disk. Linux runs c2j and
Engine together. macOS runs c2j on native APFS and connects to a sibling Linux
Engine. No workspace filesystem crosses the VM boundary. Build contexts, archive
APIs and `docker cp` transfer data explicitly. Named/anonymous volumes and tmpfs
are engine-local. Both VMs are ARM64; the primary's OS/base/resources remain the
worker's execution facts.

Runtime job-file bind mounts, file-backed Compose configs/secrets, Compose watch,
UDP, macOS-style host networking, host callbacks, arbitrary Testcontainers modules
and AMD64 emulation are outside v1. Dockerfile context/stage mounts are distinct
from runtime binds. Build secret/SSH forwarding is unqualified until separately
tested. Do not implement an API firewall: record engine-local bind behavior and
never imply that it exposes the macOS workspace. Testcontainers' engine-socket
helper mount is allowed entirely inside the disposable engine VM.

## Source and evidence baseline

- Audited upstream pulse-tart `939bb73348ae1d66049d20163f8edb908152d42e`.
  Configuration/bootstrap/launch remain macOS-specific and accept six probed
  tool capabilities. Reuse its image-update and durable handoff machinery;
  experimental Docker code is not a production allocator.
- Existing Linux live jobs proved supplied leases, concrete base/capabilities,
  renewal, Docker API/builds and private-engine isolation. Older fixtures used
  binds and therefore do not establish the new acceptance contract.
- Existing macOS probes proved authenticated Unix-socket access, IPv4 TCP
  publications, start gating, conflicts, relay restart and Go Testcontainers.
  They used an external-controller tunnel when sibling routing failed.
- Dual-virtiofs failed 14/16 read-after-write cases. It is removed from this plan.
- The available Orchard worker reports `vm-endpoints` and two VM capacity units.
  Two concurrent macOS pairs require four units, so that acceptance case cannot
  be represented by a sequential run on this worker.
- `20261106T205401Z-docker-orchard-82474`: Linux no-bind job passed (132.23 s,
  eight lease renewals). Dedicated SSH failed because `UsePAM no` rejected the
  locked service account. Keep PAM account handling with password and interactive
  authentication disabled.
- `20261106T210026Z-docker-orchard-95063`: Linux passed the additional 35-second
  blocking Docker wait (167.22 s, ten renewals). Authenticated macOS Engine access
  over an Orchard worker endpoint passed; the next probe lacked its Python image.
  Fresh-engine preparation now explicitly creates the fixed-port fixture and pulls
  that image. No macOS c2j success is inferred from endpoint success.
- Real macOS c2j execution passed in `20261106T210854Z-docker-orchard-20073`.
  SDK/registry/Java plus relay recovery passed in the explicitly supplemental
  `20261106T211953Z-docker-orchard-43781` scope. See
  [current findings](docker-no-bind-findings.md) and
  [machine-readable evidence](docker-no-bind-results.json). Neither `docker info`
  nor successful unit tests substitutes for the remaining live gates.
- Native `20261107T022409Z-docker-matrix-35171` passed: two suspended slots,
  two generations each, four real c2j jobs, fresh engine/key identities, and
  166–168 seconds of observed job overlap per generation. All ten owned VM
  records were cleaned. The worker-local bridge worked despite isolated sibling
  guest networking. This qualifies the prototype path, not production recovery.

## Implementation structure

Keep the shared provider API unchanged. Add an internal guest implementation
interface for preparation, tool/OS probes, service launch, boot identity, process
observation and shutdown. Supply Darwin/launchd and Linux/systemd implementations.
Do not branch on OS repeatedly throughout reconciliation or put OS/arch into the
shared allocation API. Audit macOS/suspend assumptions across provider/backend/guest code, including
error and recovery paths.

Extract the experimental relay/transport into production internal packages only
after stream/network qualification. Replace the prepared-one-allocation harness
with the real slot allocator; its constructor-time SSH probes, restricted Submit
options and completion-only List behavior are not the production contract.

### Library configuration

Implement these additions to the library configuration:

- `Config.Docker`: optional Docker settings; nil preserves current behavior.
- Named engine profiles: digest-pinned bootable Linux image, verified tool
  versions, CPU, memory, disk allowance and readiness timeout.
- `SlotGroup.DockerProfile`: opt-in engine profile. On Linux this configures its
  own Engine; on macOS it reserves an additional companion.
- Orchard connectivity: `WorkerEndpointAddress` reachable from guests and
  `EndpointPortRange`, with required endpoint support. Keep these outside common execution
  settings. Local Tart has equivalent host-service/network options.

Validate image/profile references, positive resource quantities, guest support,
network mode and every requested capability before provisioning. Advertise the
union of configured supported profiles per base, even while all slots are busy
or warming. `GetCapabilities` performs no SSH, preparation or resource creation.
A capability request selects only a compatible slot profile. A non-Docker job
must not acquire Docker execution facts merely because another slot supports it.

Start with the measured 4 CPU/8 GiB primary and 4 CPU/8 GiB companion, reserving
headroom before reporting c2j memory. The previous 2 CPU/4 GiB companion proposal
is unmeasured; do not make it the default until the pressure/build tests qualify
it. Count eight CPUs, sixteen GiB and two VM units for the measured pair, plus
host/relay overhead and a separately enforced disk budget. Do not count companion
resources as primary capacity. With the measured 8 GiB VM and 512 MiB reserve,
report 7680 MiB to c2j; a job requesting 8 GiB needs a larger primary profile.
Never accept that request against the smaller verified worker quantity.

### Images and readiness

Extend the current operator-driven image update scripts to prepare Linux worker
and companion images, and macOS Docker client/plugin/relay software. Resolve
c2j releases at image-preparation time using operator selection, not a library
constant. Pin input VM digests and Docker/client/plugin versions for reproducible
qualification; record actual installed versions. The companion does not need
c2j, a JobDB lease or management credentials.

Keep one distinct macOS suspended template per concurrency slot with its own
name/MAC. Freeze it before allocation credentials or live transport exist.
Use fresh cold Linux clones initially: deployed Tart 2.41.1 rejected Linux
suspension. No dependency on newer source-only support. Prepared engine images
may contain immutable cached image layers, but no allocation containers, volumes,
keys, task files or reusable engine identity. Establish exactly which Engine
identity/state paths to reset by the clone-isolation test.

Prepare software in background reconciliation. Maintain ready capacity before
claim/handoff; do not queue live leases behind apt installs or image downloads.
Admission must require both native reservations and a verified usable endpoint
before c2j starts. A warming profile remains advertised but returns NoCapacity
or Unavailable as appropriate. Check `StartBefore` again before durable handoff.

### Transport and TCP services

macOS exposes a worker-accessible Unix Docker socket, `DOCKER_HOST`, and the
conventional socket path where required by SDK discovery. The guest relay speaks
ordinary HTTP/streaming to Engine through allocation-specific SSH, with a pinned
fresh server key. Credentials live outside task artifacts and are generated
after cloning. Reconnect only before sending a request; never replay a Docker
mutation or lease delivery whose result is uncertain.

Selected Orchard transport: a worker-managed endpoint forwards to a dedicated engine SSH
port. That SSH server accepts only the allocation key and forwarding, with no
password login, shell or source-image management account. The endpoint may be
LAN reachable, but exposes authenticated transport, not a Docker HTTP socket or
container service. Validate the worker address from the primary. A successful
endpoint-listening observation is insufficient; complete SSH and Docker identity
checks. Persist the assigned endpoint port and request that exact port before
handoff, so worker endpoint recreation does not silently move a running pair.
On collision, report unavailable and reconcile; do not change a guest's endpoint
behind an active stream. Include endpoint deletion/recreation in fault tests.
No controller credentials enter either guest. Production bootstrap uses trusted
management transport; the lab harness's password login/unpinned management SSH
is not its production authentication design.

Selected local Tart transport: the equivalent worker-local TCP bridge to the
engine's dedicated SSH port, reachable at the Mac host's guest-facing address.
The no-bind test script supplies a detached helper to qualify this path. Install
and supervise the production helper with launchd; it must outlive library Close,
restore owned bindings after restart, and release them only with its allocation.
Its state stores target native UID/address and listener identity, not a job lease
or Orchard credential. Authenticate the stream end-to-end using the same guest
SSH keys as Orchard. Do not require bridged networking or change host firewall
policy as a hidden prerequisite. Native-host reachability passed the two-slot
matrix; service supervision and recovery remain implementation gates.

Map engine publications to the same IPv4 loopback port on macOS. Gate successful
start/restart on the affected container's listener readiness. Return an error and
reconcile the actual container state on bind conflict; never attach a client to
an unrelated existing listener. Watch events and periodically reconcile to remove
stale bindings and recover existing publications after relay restart. Test
multiple containers, restart policies, daemon restart, and port reuse; the current
prototype's global reconciliation can conflate an unrelated conflict with the
container being started and must be narrowed during production integration.

Do not impose a short blanket HTTP timeout on long builds, waits or streams.
Bound dialing, handshake and control-plane inventory calls separately. Ensure
upgrade/full-duplex connections and cancellation propagate; test concurrent calls
while a long stream is active. In-flight failures are reported, not transparently
retried. IPv6/UDP/callbacks require separate qualification and are not inferred
from IPv4 success.

## Durable pair lifecycle

Persist an allocation record before native creation: owner/scope, LaunchID,
JobRef, requested primary facts, selected images/profile, slot, both native names
and immutable UIDs, roles, generation and credential references. Maintain a
single allocation state machine, not two unrelated launches:

`reserved -> provisioning -> ready -> handoff-intent -> running -> reclaiming -> gone`

Failures before delivery enter reclaiming; uncertainty after handoff-intent
retains the API's unknown semantics. Never release capacity or fallback while a
worker may have received the lease. Duplicate delivery uses the existing durable
assignment fingerprint and guest intent; recovery observes rather than reruns.
Only the primary receives the lease on stdin and can obtain job secrets.

Orchard scheduling is per VM, not an atomic pair transaction. Use same-worker
labels and each VM's real resource requests. Provision ready pairs in background
with bounded partial-pair rollback; handoff is impossible until both reservations
are observed. Do not emulate reservations with zero-resource companions or
assume a free-capacity read is atomic. If multi-controller contention requires
atomic gang admission later, that is an Orchard enhancement, not a shared Pulse
allocation API change.

`List` must retain ownership until both resources are reclaimed, including a
companion whose primary exited. Return native resource items with the same
LaunchID/JobRef and keep roles/UIDs in the journal. Unreachable inventory is an
error/unknown observation, never an empty allocation or reclaimed capacity.

`Close` stops local reconciliation and releases the journal lock; it does not
kill jobs or the data path. A supervised `Run`/reconcile process reopens the same
state and reaps terminal or partial allocations. Cleanup is recoverable after a
controller crash; its latency depends on that supervisor restarting. Do not
promise an always-running independent reaper unless one is actually installed.

Reap on authoritative primary completion/failure/lease-loss termination, or
confirmed native primary disappearance. Do not infer lost lease authority from
controller disconnect, transport failure or elapsed age of a zero-timeout job.
Delete/stop companions and revoke endpoints/keys, then remove task/native disks
and release the reservation only after confirmed teardown. Preserve retryable
cleanup state when any side is unreachable. Use guest process evidence and the
existing worker termination contract; do not give the companion the JobDB lease.

## Delivery sequence and release gates

1. **Guest abstraction and images.** Linux bootstrap/worker service plus verified
   versions; cold readiness and actual c2j jobs on both backends. Preserve existing
   macOS image/update tests and no-Docker behavior.
2. **Endpoint/relay.** Qualify worker endpoint transport and native equivalent;
   build/archives/streams, Compose subset, Go and Java helpers, publication timing,
   restart/reconnect and no controller-process dependency.
3. **Pair allocator.** Typed configuration, discovery, reservations, both identities,
   handoff journal, listing and recoverable teardown. Unit-test every crash boundary
   with deterministic backends, then inject real VM/relay/engine failures.
4. **Lifecycle/performance.** Two sequential allocations per distinct macOS slot,
   two pairs concurrently, fresh credentials/state, CPU/memory/disk pressure and
   primary minima. Repeat through local Tart and external-controller Orchard.
5. **Host integration.** Export library config examples, required Orchard endpoint
   version/network settings or local helper installation, supervisor/reopen rules,
   capacity math, image update command and explicit supported/unsupported examples.
   Enable production discovery only after the selected profile passes acceptance.

The native matrix measured macOS resume-to-SSH at 10.1–14.9 seconds (four samples)
and VM cleanup at 0.19–1.01 seconds (ten samples). These are not prepared-pair
handoff latency. Remaining measurements include prepared cold Linux boot, complete
resumed pair readiness, repeated build/transfer/Compose benchmarks, and idle/loaded
resource use. Carry failed attempts into findings instead
of rewriting them as success after a fix. Test reaper behavior with normal cleanup
helpers enabled and copy APIs; no blanket Testcontainers compatibility claim.

## Next experiments and stopping criteria

`./scripts/test-macos-docker-matrix.py` passed on native Tart after fixing the
first attempt's public-key read permissions. It established the native endpoint
path, two independently named/MAC-addressed suspended templates, two jobs per
slot, distinct engine/key identities and overlapping job intervals. At most two
macOS and two Linux VMs run at once. Retain it as the native regression entry
point. Native Linux-primary c2j acceptance and the equivalent Orchard
suspended/concurrent matrix remain separate coverage gaps.

Before release, exercise the implemented paired state machine with a failure at
every transition: first VM created, second VM created, endpoint configured,
credential delivery, before/after handoff intent, during lease delivery, after
acceptance, and each deletion. Reopen the same journal after each interruption;
assert no duplicate worker, retained unknown outcome, complete native ownership,
no premature capacity release, and eventual cleanup. Repeat with backend errors
and a long-running zero-timeout job to verify disconnect does not terminate work.

Then run one pair under companion CPU saturation, companion memory exhaustion,
and a bounded private engine-disk exhaustion experiment while a second pair serves
traffic. Measure primary VM facts and workload latency, engine/relay RSS, disk
usage, failure visibility and cleanup. Use allocation-owned disks only and fixed
experiment limits. If the 4 CPU/8 GiB companion fails, tune its profile and repeat;
do not infer that the unmeasured smaller profile is adequate.

These remaining tests are explicit implementation/release gates. The Orchard
lab still has only two VM capacity units; the native concurrency result does not
qualify Orchard pair scheduling. Cross-allocation enumeration/prune isolation,
full engine-state residue checks, recovery and pressure also need explicit tests.
Do not call the whole design production-qualified until those results arrive.

Primary references: [Orchard endpoint API](https://github.com/openai/orchard/blob/main/pkg/resource/v1/endpoint.go),
[Docker build contexts](https://docs.docker.com/build/concepts/context/),
[Testcontainers copy APIs](https://java.testcontainers.org/features/files/).
