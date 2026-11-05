# Docker capability prototype

Opt-in experiments; not production Docker support. See
[usage](../../docs/docker-prototype.md),
[findings](../../docs/docker-prototype-findings.md), and
[per-experiment results](../../docs/docker-prototype-results.json).

The package implements one prepared disposable Linux allocation using the shared
provider API, plus an authenticated macOS Docker socket/TCP relay. The test
harness owns VM provisioning and cleanup. No default provider behavior changes.

```sh
go test -race ./experimental/docker/...
./scripts/test-docker-prototype.sh tart
./scripts/test-docker-prototype.sh orchard
```

Live suites deliberately exit 2 while required coverage is failed or incomplete.
