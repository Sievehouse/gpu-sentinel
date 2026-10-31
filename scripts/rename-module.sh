#!/usr/bin/env bash
# Rename the Go module once you have chosen where the repository lives, e.g.
#   ./scripts/rename-module.sh github.com/your-org/gpu-sentinel
set -euo pipefail
NEW="${1:?usage: $0 <new module path>}"
OLD="$(go list -m)"
cd "$(dirname "$0")/.."
go mod edit -module "$NEW"
grep -rl --include='*.go' "\"$OLD/" cmd internal | xargs sed -i.bak "s#\"$OLD/#\"$NEW/#g"
find cmd internal -name '*.go.bak' -delete
gofmt -l cmd internal
go build ./... && go test ./...
echo "module renamed: $OLD -> $NEW"
