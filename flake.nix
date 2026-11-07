{
  description = "macOS execution environment for Pulse Orchard guests";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/a033e66ab94a6bedf2c3d58d93a36319d3cbbbaf";
  # The preparer selects a release at runtime; Pulse does not pin c2j.
  inputs.c2j.url = "github:colony-2/c2j";
  inputs.c2j.inputs.nixpkgs.follows = "nixpkgs";

  outputs = { self, nixpkgs, c2j }:
    let
      system = "aarch64-darwin";
      pkgs = nixpkgs.legacyPackages.${system};
      # Use the selected c2j commit's source filtering and vendor hash.
      worker = c2j.packages.${system}.c2j;
      runner = pkgs.buildGoModule {
        pname = "pulse-tart-guest";
        version = "0.1.0";
        src = ./guest;
        postPatch = "cp go.mod.txt go.mod";
        vendorHash = null;
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
    in {
      packages.${system} = {
        default = pkgs.buildEnv {
          name = "pulse-worker";
          paths = [ worker runner pkgs.git pkgs.nodejs pkgs.pnpm pkgs.uv pkgs.python3 pkgs.cacert ];
          pathsToLink = [ "/bin" "/etc/ssl/certs" ];
        };
        guest = runner;
        integration = pkgs.buildEnv {
          name = "pulse-worker-integration";
          paths = [ self.packages.${system}.default fixture (pkgs.hiPrio fixtureRunner) ];
          pathsToLink = [ "/bin" "/etc/ssl/certs" ];
        };
      };
    };
}
