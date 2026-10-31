#!/bin/sh
# Runs the read-only live test in Docker. Set BIBLIOFIL_URL, BIBLIOFIL_USER
# and BIBLIOFIL_PIN (and optionally BIBLIOFIL_PICKUP, BIBSOK_PICKUP) first.
set -e
cd "$(dirname "$0")"
: "${BIBLIOFIL_URL:?set BIBLIOFIL_URL}" "${BIBLIOFIL_USER:?set BIBLIOFIL_USER}" "${BIBLIOFIL_PIN:?set BIBLIOFIL_PIN}"
docker run --rm -e BIBLIOFIL_URL -e BIBLIOFIL_USER -e BIBLIOFIL_PIN -e BIBLIOFIL_PICKUP -e BIBSOK_PICKUP -e LIVE_TEST_QUERY \
  -v "$PWD":/src -w /src -v bibliofil-gomod:/go/pkg/mod -v bibliofil-gocache:/root/.cache/go-build \
  golang:1.27-alpine go test -tags live -run Live -count=1 -v .
