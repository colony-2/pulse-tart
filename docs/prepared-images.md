# Prepare or update macOS images

Run `scripts/update-macos-images.sh` on the Apple Silicon worker from a pulse-tart checkout. Use the same account and `TART_HOME` as the Tart/Orchard worker. The command needs Tart with `clone --overwrite` (reviewed against 2.41.1), Python 3, curl, shasum and OpenSSH. Nix runs inside the guest, not on the host.

```sh
./scripts/update-macos-images.sh \
  --owner mac-pool --base macos-26-arm64 --group standard \
  --slots 2 --cpu 4 --memory 8192
```

Match these values to `Config.OwnerID` and the slot group. The default base is Tahoe; Sequoia is available as `--base macos-15-arm64`. Sizes are vCPU and MiB. Repeat for other groups/sizes. Keep one VM position free for preparation; the script builds one VM at a time but does not coordinate capacity with Pulse.

Options:

| Option | Behavior |
| --- | --- |
| `--c2j latest` | Default: resolve the latest published c2j release when preparing |
| `--c2j <tag-or-commit>` | Build that selection; branches also work |
| `--ensure` | Prepare only missing sources; reuse existing matching stopped/suspended images |
| `--image <digest-reference> --os-version <version>` | Override the catalog image and exact guest OS check together |
| `--ssh-user <user>` | Default `admin`; password from `PULSE_IMAGE_SSH_PASSWORD`, default `admin` |
| `--timeout <seconds>` | Per-VM preparation limit, default 2700 |

The guest must support SSH/passwordless sudo and reach GitHub/Nix. The script prints its temporary log directory and each preparation/publication phase. On failure it retains logs there. `PULSE_IMAGE_LOG_DIR=/path` additionally copies build/run logs to a chosen directory. It cleans up only its own staging VMs. An interrupted process that cannot run cleanup may leave a per-pool update lock; remove that lock only after verifying no updater remains active.

## Names and publication

For the example above, the canonical local names are:

```text
pulse-mac-pool-macos-26-arm64-base
pulse-mac-pool-macos-26-arm64-standard-4cpu-8192m-slot-0001
pulse-mac-pool-macos-26-arm64-standard-4cpu-8192m-slot-0002
```

Names contain the owner, base and, for slots, group/size/index. c2j versions never enter the names. Long or unusual owner/base/group values use a deterministic hashed token; `scripts/image-names.py` and the provider use the same rules.

Orchard adds `orchard-` plus a UUID/restart suffix to its native image names. The script discovers matching aliases using `tart list` and publishes replacements to those names as well as the canonical names. This requires no controller API call. When the provider first encounters script-created canonical images, direct Tart adopts them; Orchard clones them into its managed records. A missing Orchard local source probe falls back to automatic installation.

Every update starts from the configured raw OS image, installs the selected c2j once into a stopped base, then boots and suspends each requested slot with independent hardware identity. All replacements are prepared before publication. `tart clone --overwrite` takes Tart's storage lock for each replacement. A concurrent clone sees the source before or after that replacement.

Existing ordinary job clones keep their own disk/configuration/saved state and continue unaffected. A clone already created before publication also keeps the old environment even if it starts afterward. New clones use the replacement. Each new slot template has a fresh MAC and boot UUID; job startup checks its own snapshot's saved identity rather than a cached identity from the provider.

Publication is per image, not a transaction across the pool. Preparation failure leaves published sources untouched. Publication failure reports an error and can leave some sources updated; rerunning repairs the pool. Do not run suspended sources directly, use stacked clones, or modify job clones. The provider still permits only one active/unresolved job per configured slot.

## Version selection and inspection

The library's `Config.C2JVersion` selects a release only when it must install a missing base. Existing bases and templates remain authoritative. Neither a library upgrade nor changing that setting implicitly replaces them. The updater is the explicit refresh mechanism; no Pulse release or restart is required.

The selector resolves a release/tag/branch to a commit before Nix builds it. Lookup errors fail preparation; there is no fallback to a library-pinned c2j. The selected tools are available under `/opt/pulse-tart/bin`. Inspect `c2j version` inside a disposable clone to see the installed version. There is no release manifest or version sidecar. The snapshot readiness file records boot UUID/MAC for restore verification, not software version.

`build-bundle.sh` and the guest-side `prepare-image.sh` remain internal setup helpers. The archive contains shell scripts, Nix definitions and guest-launcher source. Nix fetches/builds c2j inside the VM. Normal library users and operators need only the configuration API or `update-macos-images.sh`.
