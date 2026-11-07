{
  description = "Execution environments for Pulse Tart and Orchard guests";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/a033e66ab94a6bedf2c3d58d93a36319d3cbbbaf";
  # The preparer selects a release at runtime; Pulse does not pin c2j.
  inputs.c2j.url = "github:colony-2/c2j";
  inputs.c2j.inputs.nixpkgs.follows = "nixpkgs";

  outputs = { self, nixpkgs, c2j }:
    let
      forSystem = system: let
      pkgs = nixpkgs.legacyPackages.${system};
      # Preserve the selected c2j source; preparation resolves its dependency hash.
      workerVendorHash = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";
      worker = c2j.packages.${system}.c2j.overrideAttrs (_: { vendorHash = workerVendorHash; });
      runner = pkgs.buildGoModule {
        pname = "pulse-tart-guest";
        version = "0.1.0";
        src = ./guest;
        postPatch = "cp go.mod.txt go.mod; cp go.sum.txt go.sum";
        vendorHash = "sha256-A30hPGRobiMTkKA/bDtVNiz9bNFQ/iqy7FZqLpCtHNE=";
        env.CGO_ENABLED = "0";
        postInstall = ''mv $out/bin/guest $out/bin/pulse-tart-guest'';
      };
      # Test-only gate: wait for the SSH fixture tunnel before execing the same
      # production c2j binary. Stdin is untouched; no credential enters argv/env.
      fixtureRunner = pkgs.writeShellScriptBin "c2j" ''
        wait_for_fixture=0
        for arg; do
          case "$arg" in http://127.0.0.1:18880/*) wait_for_fixture=1 ;; esac
        done
        if [ "$wait_for_fixture" = 1 ]; then
          count=0
          until /usr/bin/curl --silent --fail --max-time 2 http://127.0.0.1:18880/fixture-ready >/dev/null; do
            count=$((count + 1))
            if [ "$count" -ge 90 ]; then echo 'Fixture tunnel unavailable' >&2; exit 1; fi
            sleep 1
          done
        fi
        exec ${worker}/bin/c2j "$@"
      '';
      fixture = worker.overrideAttrs (old: {
        pname = "pulse-orchard-fixture";
        subPackages = [ "cmd/pulse-orchard-fixture" ];
        postPatch = (old.postPatch or "") + ''
          mkdir -p cmd/pulse-orchard-fixture
          cp ${./tests/integration/c2j-fixture.go.txt} cmd/pulse-orchard-fixture/main.go
        '';
        doInstallCheck = false;
      });
      dockerFixture = fixture.overrideAttrs (old: {
        pname = "pulse-docker-fixture";
        postPatch = ''
          mkdir -p cmd/pulse-orchard-fixture
          cp ${./tests/integration/docker-fixture.go.txt} cmd/pulse-orchard-fixture/main.go
        '';
        postInstall = "mv $out/bin/pulse-orchard-fixture $out/bin/pulse-docker-fixture";
      });
    in {
        docker-integration = pkgs.buildEnv {name="pulse-docker-integration";paths=[ self.packages.${system}.docker dockerFixture ];pathsToLink=[ "/bin" "/etc/ssl/certs" ];};
        worker-dependencies = worker.goModules;
        default = pkgs.buildEnv {
          name = "pulse-worker";
          paths = [ worker runner pkgs.git pkgs.nodejs pkgs.pnpm pkgs.uv pkgs.python3 pkgs.cacert ];
          pathsToLink = [ "/bin" "/etc/ssl/certs" ];
        };
        docker = pkgs.buildEnv { name = "pulse-worker-docker"; paths = [ self.packages.${system}.default pkgs.docker-client pkgs.docker-buildx pkgs.docker-compose ]; pathsToLink = [ "/bin" "/etc/ssl/certs" ]; };
        guest = runner;
        integration = pkgs.buildEnv {
          name = "pulse-worker-integration";
          paths = [ self.packages.${system}.default fixture (pkgs.hiPrio fixtureRunner) ];
          pathsToLink = [ "/bin" "/etc/ssl/certs" ];
        };
    };
    in { packages = nixpkgs.lib.genAttrs [ "aarch64-darwin" "aarch64-linux" ] forSystem; };
}
