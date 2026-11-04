# Prepare a macOS worker image

The library automatically installs pinned Nix, c2j, the guest runner, Git, Node/pnpm, uv and Python when preparing a slot. To avoid repeating installation for new slot templates, bake the same environment into a reusable base image.

Build the archive from the same checkout as the library:

```sh
make bundle
# dist/guest-bundle.tar.gz
```

Library users can also obtain the archive from `tart.GuestBundle()`. It contains the pinned flake, guest source and installation scripts, so the VM does not need a separate checkout. The library embeds these assets; a separately installed bundle is unnecessary for normal operation. `Config.GuestBundle` is an optional trusted override.

Copy and extract the archive **inside a fresh macOS ARM64 VM** with SSH, passwordless sudo and outbound GitHub/Nix access:

```sh
mkdir -p /tmp/pulse-image
tar -xzf /tmp/guest-bundle.tar.gz -C /tmp/pulse-image
sudo /bin/bash /tmp/pulse-image/prepare-image.sh --shutdown
```

`PULSE_IMAGE_PREPARED` confirms preparation. The script preserves the Nix daemon, `/nix`, the `/opt/pulse-tart` profile, its environment fingerprint and the unprivileged worker account. It installs no worker service and accepts no lease. Repeating preparation on the unused builder is safe. A VM containing execution state or runtime services is rejected; this script does not clean used workers.

After clean shutdown, publish from the Mac host using your existing image pipeline. For example:

```sh
tart login registry.example.com
tart push pulse-image-builder registry.example.com/team/pulse-macos:v1
```

Keep registry credentials outside the image. Publishing is a separate operator action. Set `Base.Image` to the resulting manifest digest, `Base.OSVersion` to the actual guest version, and `Base.Name` to a concrete name such as `macos-sequoia-15.7.7-arm64`. Set SSH credentials to those baked into the image; configure registry pull access on the Orchard worker or local Tart host.

The library verifies the prepared environment fingerprint and tool paths. A match skips installation; a mismatch rebuilds the pinned environment. Changing c2j, Nix inputs, guest code or installation settings requires rebuilding the image to retain that shortcut. Never edit fingerprints to force a match.

Each job still uses a fresh clone. Snapshot restore reuses the slot's prepared disk and memory; it does not rerun a previously executed guest. Nix stores are guest-local. Historical measurements were approximately 17.7 seconds for a fresh stopped-image clone and 8.2 seconds for a prepared suspended clone; these exclude initial image download and template preparation and are not job-completion times. See [snapshot measurements](suspended-templates.md).
