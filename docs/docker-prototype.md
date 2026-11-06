# Experimental Docker allocations

This prototype is isolated in `experimental/docker`. Normal macOS provider
configuration and capabilities are unchanged. It includes the demonstrated Linux
Docker allocation/API bridge and a new macOS c2j shared-workspace path awaiting
live Mac validation. **It does not implement the complete production macOS Docker
allocation contract.** See
[detailed findings](docker-prototype-findings.md) before using the results.

## Investigate shared-file consistency first

The latest run (`20261106T201055Z-docker-tart-86731`) passes tool/permission
admission but fails a shared-file content assertion before c2j starts. Run the
smaller reproduction before another complete job suite:

```sh
./scripts/test-macos-docker-filesystem.sh
```

This builds only the SSH helper and boots two disposable VMs with the same
virtiofs setup. It does not build c2j, macOS Docker CLI/plugins, or a JobDB fixture.
It checks both directions, reader-created and writer-created files, same-size
and growing overwrites, explicit fsync, and atomic replacement. The report
records first-read bytes/hashes, host backing-file observations, delayed reads,
and diagnostic remounts. A failed first read stays failed even if waiting or
remounting makes it correct later. These are synthetic fixtures, not user files.

If plain guest sharing passes, it installs Docker Engine in its private Linux
VM and tests container creation/overwrite through a normal bind mount. If plain
sharing fails, Docker setup is skipped and explicitly marked `not_run`.

Inspect `filesystem-coherence.json` in the generated archive. Scope is
`filesystem-coherence`, with explicit required cases and `complete: false` for
the full contract. This is a diagnostic, not a workaround: ordinary jobs cannot
be required to remount their workspace or sleep after every file write. VM
cleanup and relative `test-results/...` output follow the other scripts.

## Focused macOS execution test

On the Apple Silicon Mac, run:

```sh
./scripts/test-macos-docker.sh
```

This prepares one macOS template, then runs two fresh macOS/Linux pairs
sequentially. Each macOS primary executes a real supplied JobDB lease through
`NewMacOS` and the shared provider API. It installs native CLI/Buildx/Compose,
runs c2j as `pulseworker`, and checks real builds, Compose, SDK forwarding,
checkout and TMPDIR binds against an allocation-private Linux engine. Native
CLI/Buildx are built from upstream source; Compose uses its upstream Darwin
release with its published SHA256 verified (its watcher requires cgo).

To test warm macOS clones from the same model:

```sh
./scripts/test-macos-docker.sh --lifecycle suspended
```

The default is `--lifecycle cold`. Suspended mode must preserve the template's
boot identity and shared mount while running both real jobs; it cannot silently
fall back to cold boot. Linux always cold-boots. Optional `--slots 2` exercises
two distinct templates **sequentially**, not two concurrent pairs; the latter
remains unverified. Each job has a fresh primary and companion.

A slot owns a stable host share directory for the lifetime of its template.
The provider mounts it at `/Users/Shared/pulse/work` in both guests and places
jobs under `jobs/<allocation>/tmp`. It never swaps or renames the share root.
Only after both job VMs are deleted does it remove that job's directory. The
next generation must see no old job directory, and the root inode must match.
Cleanup rejects symlinked/replaced roots and `jobs` directories.

A focused report declares `scope: macos-workspace` and lists `required_cases`.
Exit 0 means this workflow passed; `complete: false` still means the **full**
prototype contract has outstanding cases. Filesystem diagnostics (including
locking and case behavior), UDP, callbacks and the broader failure matrix are
not silently declared passing. Full-suite behavior remains unchanged. Both
report and archive messages use paths beginning with `test-results/`.

The new flow has been built and unit-tested on Linux, including cross-compiling
macOS c2j/tools. It has not yet been run on the Mac. Build/download time varies;
the two jobs themselves deliberately wait 65 seconds each to exercise lease
renewals. Send the generated archive after running it.

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
counted as passing. The full-contract suite is expected to exit 2 even where all
implemented checks work. The focused suite above reports its narrower scope. The full Orchard run took about nine minutes; image
pulls/package downloads and failures can add several minutes. Every guest command
and polling stage is bounded. A returned local Tart run confirmed basic shared visibility; see the findings.

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

`docker.NewMacOS` additionally takes the companion Guest/native identity and a
per-allocation Workspace. Construction checks the concrete macOS version,
primary resources, matching engine identity, and a primary/companion/container
filesystem round-trip. c2j receives only the primary CPU/memory and concrete
macOS base. Both VMs appear in List with the same LaunchID/JobRef; the lease goes
only to macOS. This remains a prepared-pair adapter, not an automatic pool.

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
full cross-guest filesystem/snapshot matrix in the full diagnostic suite. The focused suite now implements real macOS c2j
execution and stable-root cold/suspended clones, pending the next Mac run. Orchard
shared-workspace tests are blocked by missing scoped workspace provisioning and
disabled hostDirs in the available lab. No SSHFS/NFS fallback was substituted.
