# Module 1 (coldmail-verify) control plane, built from the PINNED checkout for the platform harness.
#
# WHY THIS FILE LIVES HERE AND NOT IN MODULE 1: module 1 ships no Dockerfile and no compose file for its control plane (its own CI builds it natively with
# `go build ./...` and applies migrations with a psql loop — Docs/phase-0-spec.md F2). The harness must run it as a real service on a private network, so the
# PLATFORM supplies the recipe. Module 1 is not modified: the build context is the unmodified pinned checkout and this file stays outside it.
#
# Build context = the root of the coldmail-verify checkout (so `control-plane/` is a sibling directory of nothing else we copy).
# The base images are the same digests the modules' own Dockerfiles pin (services/control/Dockerfile in coldmail-send-managed).
FROM golang:1.26-alpine@sha256:87c65a688975c0b1d4585d204874c45ca78354670e3a5f11368c5a492303d696 AS build
WORKDIR /src
COPY control-plane/go.mod control-plane/go.sum ./
RUN go mod download
COPY control-plane/ ./
RUN CGO_ENABLED=0 GOTOOLCHAIN=local go build -o /out/control-plane ./cmd/control-plane

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache ca-certificates curl && adduser -D -u 10001 verify
USER verify
COPY --from=build /out/control-plane /usr/local/bin/control-plane
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/control-plane"]
