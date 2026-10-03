#!/usr/bin/env bash
set -euo pipefail
:  "${JWT_SECRET:?set JWT_SECRET to the servers secret}"
go run ./cmd/burst -url "${1:?usage: ./burst.sh <BASE_URL>}" "${@:2}"