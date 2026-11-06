# Experimental Docker allocations

This prototype is isolated in `experimental/docker`. Normal macOS provider
configuration and capabilities are unchanged. It includes Linux Docker execution
and a macOS/private-Linux-engine prototype using native, unshared workspaces. **It does not implement the complete production macOS Docker
allocation contract.** See
[detailed findings](docker-prototype-findings.md) before using the results.

## Current no-bind qualification

The current contract is [Docker without bind mounts](../pulse-tart-docker-capability-requirements.md).
See the [implementation plan](docker-capability-plan.md) for selected architecture,
remaining release gates and source audit. Production defaults are unchanged.

On the Apple Silicon worker:

```sh
./scripts/test-macos-docker.sh --rounds 2
```

From an external controller against Orchard:

```sh
PULSE_TEST_ORCHARD_URL=http://192.168.1.195:6120 \
PULSE_TEST_ORCHARD_WORKER=jnmac25.local \
PULSE_TEST_ENGINE_WORKER_ADDRESS=192.168.1.195 \
./scripts/test-docker-prototype.sh orchard --no-bind --rounds 2
```

The worker address must be reachable from the guests; it need not be the controller
address. Orchard must support VM endpoints. The endpoint exposes a dedicated
allocation-key-only SSH service, never the image's password-based management SSH.
Native Tart uses a detached worker-local TCP bridge for this experiment; production
service supervision remains a separate implementation gate.

Each round runs a Linux c2j allocation, deletes it, then runs a macOS/Linux pair.
By default this exercises sequential cold allocations. For native suspended
clones use `--no-bind-platform macos --lifecycle suspended --rounds 2`; the script
prepares a credential-free template, verifies preserved boot/MAC identity and
absence of previous job/key files, and checks fresh engine identities.

For two concurrent native suspended slots, each with two generations:

```sh
./scripts/test-macos-docker-matrix.py
```

That matrix needs capacity for four running VMs (16 vCPUs, 32 GiB plus host
overhead). Expect 15–30 minutes plus downloads. It checks distinct template MACs,
engine IDs and transport keys and requires overlapping job intervals. Each child
runs the full job/registry/SDK scope. Both child archives are included in its
relative `test-results/...` archive. The native matrix has not been run here;
it does not claim the broader pressure/fault matrix is complete.

Unsupported combinations fail before provisioning.
Tools are rebuilt from resolved c2j release source and recorded with the report.
Each job includes deliberate waits for lease renewal, streaming and long API calls;
allow several minutes per round plus builds/downloads and Java installation.

The fixture checks transferred build context and `.dockerignore`, executable files,
8 MiB binary archive copies including stopped containers, named volumes, tmpfs,
Compose builds/network/health/ports/env_file, SDKs and Go/Java Testcontainers copies
with normal cleanup helpers. A private registry checks synthetic credential use.
Unsupported binds are observed, not universally rejected. Reports distinguish this
focused scope from the full production contract, which remains incomplete.

Archives and report paths are printed relative to `test-results/`. Do not run
concurrent suites against a worker limited to two VM units. No host directories,
shared filesystems or host Docker engine are used.

## Historical filesystem experiment

`./scripts/test-macos-docker-filesystem.sh` remains available to reproduce the
14/16 coherence failures. It is outside the new contract. The old shared-workspace
execution mode is disabled; neither delayed reads nor remounts are a workaround.

## Requirements and reports

The controller needs Go (the module toolchain), Python 3.9+, curl, tar and
ssh-keygen, with access to GitHub/GHCR, Docker Hub, Ubuntu packages, Homebrew and
Maven Central. Guest management uses the public disposable lab image login;
production images need separate hardened management access. The Python Orchard
harness targets the unauthenticated lab controller, not production authentication.

`PULSE_TEST_C2J_VERSION` selects a release or commit; default `latest` resolves once
and records the commit. `PULSE_TEST_TART_HOME` selects a local Tart home and
`PULSE_TEST_SSH_PASSWORD` overrides the lab image password. For focused iteration,
`--no-bind-platform linux` or `--no-bind-platform macos` explicitly narrows required
coverage; omitted platforms stay unrun. The default is `both`.

Each entry point snapshots source, records hashes/versions, captures logs and
writes an ignored archive. Exit 2 means failed/incomplete requested coverage.
Scoped success applies only to `required_cases`; `complete: false` means production
acceptance remains outstanding. Older full/relay-only modes retain historical
research coverage; use `--no-bind` for the current requirements.

## Cleanup and private state

The suite stops/deletes only resources whose recorded native UID (Orchard) or
directory inode (Tart) still matches. Ownership journals, leases and engine keys
are in a private temporary directory outside report archives. Its path is in
`report.json` as `private_state`. Retry an interrupted cleanup with:

```sh
python3 scripts/docker-prototype/cleanup.py /tmp/pulse-docker-private-REPLACE
```

The command is repeatable and refuses changed identities. After successful VM
cleanup it also stops recorded detached native endpoint helpers, verifying their
process command and private state identity. Keep that directory
private. After successful VM cleanup, remove it manually when inspection is
finished; it may contain the test workspace and private credentials. Orchard
API removal is verified, but host filesystem removal is not independently
attested. No name-prefix or global cleanup is performed.

## Library shape

`docker.NewLinux(ctx, docker.Config{Enabled: true, ...})` implements the existing
`pulse-provider-api.Provider`. Its explicit configuration describes **one already
prepared, disposable Linux VM**, not a production pool. The harness provisions
and removes that VM. Construction verifies OS/architecture/resources and Engine,
Buildx, Compose, git and c2j availability. Read-only discovery advertises the
concrete Ubuntu base with `docker` and `git`.

Submit validates constraints, journals an uncertain intent before delivery,
forwards the supplied opaque lease through SSH stdin, and launches c2j as
`pulseworker`. Reopening the journal and submitting the same assignment does not
execute it twice. A second LaunchID cannot reuse that VM. c2j gets the concrete
base and verified allocation facts. TMPDIR places its actual checkout under
`/Users/pulseworker/work/tmp`; engine storage, credentials and lease delivery
remain outside that tree. Cancellation/abandonment reconciliation is not yet a
production service: the harness owns teardown.

`docker.NewMacOS` additionally takes the companion Guest/native identity and a
per-allocation Workspace. Construction checks the concrete macOS version,
primary resources and matching engine identity through the authenticated relay.
The workspace stays private to the primary; no cross-guest file probe runs. c2j receives only the primary CPU/memory and concrete
macOS base. Both VMs appear in List with the same LaunchID/JobRef; the lease goes
only to macOS. This remains a prepared-pair adapter, not an automatic pool.

The macOS relay exposes a real local Unix socket and conventional
`/var/run/docker.sock`. A fresh per-allocation SSH key and pinned engine host key
protect the engine connection. In the no-bind mode a worker-local endpoint carries
that encrypted stream, without depending on the external controller process.
Container start success is gated on binding its published IPv4 TCP ports on macOS;
conflicts fail the start and stop that container. UDP, reverse callbacks and full
Docker compatibility remain outside the qualified subset.
