# Experimental Docker allocations

This prototype is isolated in `experimental/docker`. Normal macOS provider
configuration and capabilities are unchanged. It establishes a working Linux
Docker allocation and a macOS-to-Linux Docker API/network bridge. **It does not
implement the complete macOS Docker allocation contract.** See
[detailed findings](docker-prototype-findings.md) before using the results.

## Run real-machine experiments

Requirements on the controller: Go (the module/toolchain versions in `go.mod`),
Python 3.9+, curl, tar, and ssh-keygen; network access to GitHub, GHCR, Docker Hub,
Ubuntu packages and Maven Central. Guest images use the public `admin/admin`
lab login. Use disposable infrastructure; this is not a hardened deployment.
The suite downloads tools and images and executes real containers and c2j leases.

On an Apple Silicon Mac with Tart installed:

```sh
./scripts/test-docker-prototype.sh tart
```

From an external controller against the tested Orchard lab:

```sh
PULSE_TEST_ORCHARD_URL=http://192.168.1.195:6120 \
PULSE_TEST_ORCHARD_WORKER=jnmac25.local \
./scripts/test-docker-prototype.sh orchard
```

Optional: set `PULSE_TEST_C2J_VERSION` to a tag or commit; the default resolves
`latest` once and records its commit. `PULSE_TEST_TART_HOME` selects a local Tart
home. `PULSE_TEST_SSH_PASSWORD` overrides the lab image password. The Python
Orchard driver currently supports this unauthenticated lab controller, not
production controller authentication.

Both entry points snapshot the source they test, write source hashes and
`report.json`, capture logs, and produce an ignored `test-results/*.tar.gz`.
Exit **2** means failed or incomplete requested coverage. An unrun case is never
counted as passing. The current prototype is expected to exit 2 even where all
implemented checks work. The full Orchard run took about nine minutes; image
pulls/package downloads and failures can add several minutes. Every guest command
and polling stage is bounded. Local Tart has not been executed here.

For a shorter API/network-only check (still explicitly incomplete):

```sh
./scripts/test-docker-prototype.sh orchard --relay-only
```

The full suite creates two Linux allocations concurrently, runs two generations,
and tests a macOS/Linux pair after releasing one Linux VM. Each guest reserves
4 CPUs, 8 GiB and one Orchard VM unit. It never attaches the host Docker socket,
changes Orchard security settings, or prunes unrelated engines/images.

## Cleanup and private state

The suite stops/deletes only resources whose recorded native UID (Orchard) or
directory inode (Tart) still matches. Ownership journals, leases and engine keys
are in a private temporary directory outside report archives. Its path is in
`report.json` as `private_state`. Retry an interrupted cleanup with:

```sh
python3 scripts/docker-prototype/cleanup.py /tmp/pulse-docker-private-REPLACE
```

The command is repeatable and refuses changed identities. Keep that directory
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

The macOS relay exposes a real local Unix socket and conventional
`/var/run/docker.sock`. A fresh per-allocation SSH key and pinned engine host key
protect the engine connection. When sibling networking is isolated, an explicit
lab tunnel passes that end-to-end SSH stream through the external controller.
It carries Docker traffic, not workspace files. A production worker-local
transport is preferable. Container start success is gated on binding its
published IPv4 TCP ports on macOS; conflicts fail the start and stop that
container. UDP, reverse callbacks and full Docker compatibility are unfinished.

The local Tart suite includes initial virtiofs visibility, filesystem primitives,
host-directory replacement and suspend probes. It does not yet implement the
full cross-guest filesystem/snapshot matrix or real macOS c2j execution. Orchard
shared-workspace tests are blocked by missing scoped workspace provisioning and
disabled hostDirs in the available lab. No SSHFS/NFS fallback was substituted.
