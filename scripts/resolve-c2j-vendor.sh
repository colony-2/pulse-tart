#!/bin/bash
# Resolve the selected source's Go dependency hash during image preparation.
# This patches only the disposable build recipe, never an installed version file.
set -euo pipefail
set +x
bundle=$1
log=$(mktemp)
trap 'rm -f "$log"' EXIT
if nix --extra-experimental-features 'nix-command flakes' build --no-link --no-write-lock-file "$bundle#worker-dependencies" >"$log" 2>&1; then exit 0; fi
hash=$(sed -n 's/.*got:[[:space:]]*\(sha256-[A-Za-z0-9+\/=]*\).*/\1/p' "$log")
[[ $hash =~ ^sha256-[A-Za-z0-9+/]{43}=$ ]] || { tail -c 8000 "$log" >&2; exit 1; }
sed "s|workerVendorHash = \"[^\"]*\";|workerVendorHash = \"$hash\";|" "$bundle/flake.nix" > "$bundle/flake.nix.tmp"
mv "$bundle/flake.nix.tmp" "$bundle/flake.nix"
nix --extra-experimental-features 'nix-command flakes' build --no-link --no-write-lock-file "$bundle#worker-dependencies"
