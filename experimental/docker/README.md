# Docker capability prototype

Opt-in experiments; not production Docker support. See
[usage](../../docs/docker-prototype.md),
[findings](../../docs/docker-prototype-findings.md), and
[per-experiment results](../../docs/docker-prototype-results.json).

The package implements prepared disposable Linux allocations and macOS/Linux
pairs using the shared provider API, plus an authenticated Docker socket/TCP relay. The test
harness owns VM provisioning and cleanup. No default provider behavior changes.

```sh
go test -race ./experimental/docker/...
./scripts/test-docker-prototype.sh tart
./scripts/test-docker-prototype.sh orchard
```

Live suites deliberately exit 2 while required coverage is failed or incomplete.

The new focused Mac path runs real c2j leases on a shared workspace:

```sh
./scripts/test-macos-docker.sh
./scripts/test-macos-docker.sh --lifecycle suspended
```

Focused success applies only to the report's explicit `required_cases`; full
production readiness is still reported separately as `complete: false`.
