#!/bin/bash
# Resolve a release for installation; the checkout itself has no c2j pin.
set -euo pipefail
set +x
bundle=$1
version=$2
if [[ $version != bundled ]]; then
  [[ $version =~ ^[A-Za-z0-9][A-Za-z0-9._/-]*$ ]] || { echo 'Invalid c2j version.' >&2; exit 2; }
  response=$(mktemp)
  trap 'rm -f "$response"' EXIT
  json_field() {
    if [[ $(uname -s) == Darwin ]]; then
      /usr/bin/plutil -extract "$1" raw -o - "$response"
    else
      python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$response" "$1"
    fi
  }
  if [[ $version == latest ]]; then
    curl --fail --silent --show-error --location --retry 3 --max-time 60 \
      https://api.github.com/repos/colony-2/c2j/releases/latest -o "$response"
    version=$(json_field tag_name)
    [[ $version =~ ^[A-Za-z0-9][A-Za-z0-9._/-]*$ ]] || { echo 'Invalid c2j release tag.' >&2; exit 1; }
  fi
  if [[ $version =~ ^[0-9a-f]{40}$ ]]; then
    revision=$version
  else
    curl --fail --silent --show-error --location --retry 3 --max-time 60 \
      "https://api.github.com/repos/colony-2/c2j/commits/$version" -o "$response"
    revision=$(json_field sha)
  fi
  [[ $revision =~ ^[0-9a-f]{40}$ ]] || { echo 'Invalid c2j commit returned by GitHub.' >&2; exit 1; }
  # Keep nixpkgs locked. Nix resolves c2j's changed input from this exact commit.
  sed -E "s|inputs\.c2j\.url = \"github:colony-2/c2j[^\"]*\";|inputs.c2j.url = \"github:colony-2/c2j/$revision\";|" \
    "$bundle/flake.nix" > "$bundle/flake.nix.tmp"
  grep -Fq "inputs.c2j.url = \"github:colony-2/c2j/$revision\";" "$bundle/flake.nix.tmp"
  mv "$bundle/flake.nix.tmp" "$bundle/flake.nix"
fi
revision=$(sed -nE 's|.*inputs\.c2j\.url = "github:colony-2/c2j/([0-9a-f]{40})";.*|\1|p' "$bundle/flake.nix")
[[ $revision =~ ^[0-9a-f]{40}$ ]] || { echo 'Bundle has no resolved c2j commit.' >&2; exit 1; }
echo "c2j: $revision ($version)" >&2
