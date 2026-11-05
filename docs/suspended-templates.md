# Suspended concurrency slots

Each slot owns a separate, unused, suspended macOS template. Its jobs run one at a time in fresh clones. Different slots have independently generated MACs and unique template names, allowing simultaneous clones without sharing a network identity. The slot's original template never executes a job or receives a lease.

Tart's suspended clones preserve saved memory and MAC identity. Changing hardware after cloning can invalidate the snapshot; an active MAC collision can also cause cold-boot fallback. Both backends preserve slot sizing, reserve a slot before creation, and check restored boot UUID/MAC before delivering the lease. Direct Tart additionally checks running native VMs for MAC collisions. Do not run a template or mutate provider-owned VMs outside the library.

Orchard needs the configuration-preserving suspended-clone implementation from [upstream PR #473](https://github.com/cirruslabs/orchard/pull/473). The lab's older 0.55.0 worker failed restoration with `VZErrorDomain Code=12`; the user-updated build succeeded. Its API reports version and commit as `unknown`, so validation does not establish an exact installed release.

Historical lab measurements, 4 vCPU / 8 GiB:

| Operation | Observed time | Boundary |
| --- | ---: | --- |
| Cached bare image cold boot | 15.6 s mean | Create request to first guest command; no bootstrap |
| Prepared stopped clone | 17.7 s mean | Create request to c2j/Nix readiness |
| Prepared suspended clone | 8.2 s mean | Create request to c2j/Nix readiness; three sequential clones |
| New library's two-template preparation | 167.2 s | Reconciliation to both templates ready and suspended |
| New library's two simultaneous clones | 20.1 s | Submission through handoff, invalid-lease rejection and shutdown |

These have different measurement boundaries. Image pulls and template preparation are excluded from clone timings. Polling adds roughly one to two seconds. Those lifecycle timings used intentionally invalid leases. The full Orchard suite now also verifies real JobDB completion, Nix execution, secret retrieval, renewal beyond the initial lease lifetime, and completed/expired lease rejection. The validated run took 389.2 seconds including template preparation and cleanup; see [validation records](../compatibility.json).

Tahoe 26.6.2 also passed the full Orchard suite on 2026-11-05: 418.3 seconds total, 212.5 seconds preparing two templates, and 16.1 seconds for concurrent clone handoff, invalid-lease rejection and shutdown. The real JobDB job completed with six renewals and secret retrieval; replay/expiry rejection and cleanup passed. Direct Tart validation for Tahoe remains pending.

Close preserves templates and active jobs. Reopening the same configuration/state recovers them. Reconfiguration drains changed slots; `Drain` retires templates after jobs finish. Lost create responses retain their reservation, failed preparation remains inspectable, and identity mismatches are quarantined. VM age alone never authorizes cleanup of a zero-timeout worker.

Direct Tart implements the same model using detached local runs and SSH delivery. Its command/ownership/recovery tests run without macOS. The full suite, including real JobDB execution and cleanup, passed on the user’s Apple Silicon Mac with Tart 2.41.1 on 2026-11-04. The live test took 368.1 seconds, including 191.0 seconds preparing templates; JobDB service startup took 7 seconds. Run it using `./scripts/test-macos.sh` from the [README](../README.md). Host/worker reboot recovery of saved memory is not guaranteed across incompatible macOS or Tart upgrades; drain and rebuild affected templates.
