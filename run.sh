#!/bin/bash
set -euo pipefail

script=${BASH_SOURCE[0]}
while [[ -L "$script" ]]; do
    directory=$(cd -- "$(dirname -- "$script")" && pwd -P)
    script=$(readlink "$script")
    [[ "$script" == /* ]] || script="$directory/$script"
done
root=$(cd -- "$(dirname -- "$script")" && pwd -P)
cd -- "$root"

if ! command -v go >/dev/null 2>&1; then
    printf '%s\n' 'Go is required. Install Go with toolchain auto-download enabled (GOTOOLCHAIN=auto), or install Go 1.27.1.' >&2
    exit 127
fi

mkdir -p -- bin
if go build -o bin/cloud-storage-migration ./src >&2; then
    exec "$root/bin/cloud-storage-migration" "$@"
else
    status=$?
    printf '%s\n' 'Build failed; no binary was run. Use Go 1.27.1 or enable toolchain auto-download with GOTOOLCHAIN=auto.' >&2
    exit "$status"
fi
