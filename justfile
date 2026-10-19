# airut dev tasks. `just` lists them.
#
# `just ci` runs every check a source checkout can run.
# `just ci-internal` also checks the internal ticket store and the release
# tooling, and is what the internal CI workflow runs.

set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

# Everything is built and tested without cgo and on the pure-Go Olm backend,
# which the Matrix adapter's rihma requires. A build without the tag fails on
# purpose; see guard_nogoolm.go.
export CGO_ENABLED := "0"
tags := "-tags goolm"

# Every GOOS/GOARCH pair that must build without cgo.
targets := "linux/amd64 linux/arm64 darwin/arm64 windows/amd64"

default:
    @just --list

# Build every package for this platform.
build:
    go build {{tags}} ./...

# Run the tests.
test:
    go test {{tags}} ./...

# go vet, also over the live tests behind the e2e tag.
vet:
    go vet {{tags}} ./...
    go vet {{tags}},e2e ./matrix/

# Rewrite sources with gofmt.
fmt:
    gofmt -w .

# Fail if gofmt would change a file.
fmt-check:
    @diff=$(gofmt -l .); if [ -n "$diff" ]; then echo "gofmt issues in:"; echo "$diff"; exit 1; fi

# Fail if mattn/go-sqlite3 is linked. Without cgo it compiles to a stub that
# fails only at runtime, so a build passing proves nothing about it.
nocgo-check:
    @if go list -deps {{tags}} ./... | grep -qx github.com/mattn/go-sqlite3; then \
        echo "mattn/go-sqlite3 is in the cgo-free build; open the store on the pure-Go driver instead"; \
        go mod why -m github.com/mattn/go-sqlite3; exit 1; \
    fi

# Fail unless a build without -tags goolm fails, naming the guard package.
guard-check:
    @if out=$(go build ./... 2>&1); then \
        echo "a build without -tags goolm succeeded; guard_nogoolm.go should stop it"; exit 1; \
    fi; \
    if ! grep -q buildwithtagsgoolm <<<"$out"; then \
        echo "a build without -tags goolm failed without naming the guard:"; echo "$out"; exit 1; \
    fi

# Fail if the committed sqlc output in agent/sessions/dbq differs from what
# its queries and migrations generate, including a file sqlc would add or
# remove. It regenerates into a scratch copy with the command in gen.go, so
# the pin lives in one place and the tree is left alone. CGO_ENABLED=0 holds
# here too, which proves sqlc still parses through WebAssembly.
sqlc-check:
    #!/usr/bin/env bash
    set -euo pipefail
    gen=$(sed -n 's|^//go:generate ||p' agent/sessions/gen.go)
    tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
    cp -r agent/sessions/sqlc.yaml agent/sessions/migrations agent/sessions/queries "$tmp"/
    (cd "$tmp" && $gen)
    if ! diff -r "$tmp/dbq" agent/sessions/dbq; then
        echo "agent/sessions/dbq is stale; run go generate ./agent/sessions and commit the result"; exit 1
    fi

# vet, gofmt, the cgo leak check, the goolm guard, and the sqlc output.
lint: vet fmt-check nocgo-check guard-check sqlc-check

# Build every package for every target.
cross:
    for t in {{targets}}; do \
        echo "== $t"; \
        GOOS=${t%/*} GOARCH=${t#*/} go build {{tags}} ./...; \
    done

# Start a throwaway Synapse for the Matrix live suite, on
# 127.0.0.1:18118 (SYNAPSE_PORT). Uses podman, or docker via
# CONTAINER_ENGINE. Never a real account or homeserver.
synapse:
    testing/synapse/harness.sh up

# Stop the throwaway Synapse; its state is kept for the next start.
synapse-down:
    testing/synapse/harness.sh down

# Stop the throwaway Synapse and wipe its state.
synapse-clean:
    testing/synapse/harness.sh clean

# The Matrix adapter's live suite against the throwaway Synapse. ci builds
# the e2e files through vet; only this runs them.
e2e: synapse
    AIRUT_E2E_HS="http://127.0.0.1:${SYNAPSE_PORT:-18118}" go test {{tags}},e2e -count=1 -timeout 10m -v ./matrix/

# Validate the ticket store without writing anything.
tickets:
    git ticket check --fix --dry-run --strict

# The public release tooling's offline tests, and the leak check over the
# public tree HEAD would publish. Both must pass. release/README.md is the
# procedure.
release-tools:
    cd release && python3 -B -m unittest -q
    python3 -B release/publish.py candidate HEAD

# Every check a source checkout can run.
ci: lint test cross

# The internal CI gate: ci, plus the ticket store and the release tooling.
ci-internal: ci tickets release-tools
