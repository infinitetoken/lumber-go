#!/bin/sh
# Copies Lumber-Kit's contract (schema.json, vectors.json) into testdata/contract at the kit version in
# testdata/contract/VERSION, from the npm registry, or from a local Lumber-Kit checkout with --from while the kit's change
# isn't released yet.
#
#   scripts/sync-contract.sh                    # the pinned version
#   scripts/sync-contract.sh 0.8.0              # pin a new version
#   scripts/sync-contract.sh --from ../Lumber-Kit
#
# CI runs it and fails when the copy differs from the pinned version's.
set -eu
contract="$(cd "$(dirname "$0")/.." && pwd)/testdata/contract"

if [ "${1:-}" = "--from" ]; then
  cp "$2/contract/schema.json" "$2/contract/vectors.json" "$contract/"
  echo "Copied the contract from $2 (VERSION unchanged)"
  exit 0
fi

version="${1:-$(cat "$contract/VERSION")}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
curl -fsSL "https://registry.npmjs.org/@infinitetoken/lumber-kit/-/lumber-kit-$version.tgz" | tar -xz -C "$work" package/contract
cp "$work/package/contract/schema.json" "$work/package/contract/vectors.json" "$contract/"
echo "$version" > "$contract/VERSION"
echo "Synced the contract from Lumber-Kit $version"
