# Pulse provider for Orchard and Tart

This cell owns `github.com/colony-2/pulse-tart`, an importable Go library implementing the shared cell API that executes c2j workers in disposable VMs. `SPEC.md` defines the implementation contract.

Support Orchard's public REST API and direct local Tart for macOS ARM64 guests. Orchard library users can run on Linux or macOS; direct Tart requires an Apple Silicon Mac with Tart installed. Linux guests and shared volumes are deferred.

Own approved image mapping, provisioning, automatic guest bootstrap, process and lease delivery, admission, active owned-instance listing, durable reconciliation, cleanup, packaging, and automated integration tests. Install Nix on the base VM, then use a locked Nix environment for c2j, the guest runner, and execution tools. Each phase 1 VM has its own Nix store and caches. Support unattended bootstrap through Orchard so routine tests need no manual guest setup.

Use `github.com/colony-2/pulse-provider-api`; do not copy private protocols or import Pulse internals. Pulse owns scheduling, placement, lease acquisition, provider lifecycle, and failed-deployment retries. c2j owns execution, lease renewal, and retrieving application secrets using the supplied lease. JobDB owns durable job state. The provider does not provision the Mac fleet or build another scheduler.

Construct the same c2j supplied-lease execution mode as Docker from JobRef and lease. Pass the lease unchanged through stdin as the only worker credential; never inject application secrets or log lease bytes. Acceptance means native provisioning was dispatched successfully, not that the VM booted or c2j started. Ambiguous provisioning remains unknown without replay or fallback; repeated launch IDs never start another worker.

Expose one shared typed configuration API for bases, slot counts, CPU/memory sizes, state and bootstrap. Orchard adds connectivity and worker placement. Remove provider CLI, plugin framework and configuration-file support. Each slot owns a distinct suspended template with a unique MAC; at most one disposable job clone may use it at a time. Preserve ownership and reservations across library/host restarts.

Use durable ownership and reservations across library instances. Listing is observational, includes only active owned instances, and supports exact launch filters and pagination. Unsupported requirements must be declined before dispatch. Accepted execution survives library closure. After startup grace, clean up when c2j is confirmed absent; a running zero-timeout worker has no implicit runtime cap. Confirm Orchard API-record deletion to complete cleanup; physical removal remains Orchard's responsibility. Never delete unrelated resources.

Require deterministic API/HTTP/state tests and automated real Orchard tests covering bootstrap, supplied-lease execution, Nix-managed tools, listing, failures, restart recovery, and cleanup. Deferred features are not phase 1 release gates. The development implementation and measured compatibility are documented in `README.md` and `compatibility.json`.

Implementation releases follow FSL-1.1-ALv2, including its two-year Apache-2.0 future grant, with appropriate new-work attribution and release dates. Retain upstream notices for copied material and recheck actual release/dependency licenses before distribution. Do not describe current implementation releases as immediately Apache-2.0 or MIT.
