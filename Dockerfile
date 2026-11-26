# The platform image: the service, its migrator and the harness probe, as three static binaries in one small image. Built by the harness (tests/harness/run.sh)
# and, later, by the deployment. Base images are pinned by digest (the same digests the three modules pin); CI builds with --pull --no-cache.
FROM golang:1.26-alpine@sha256:87c65a688975c0b1d4585d204874c45ca78354670e3a5f11368c5a492303d696 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
# The build tags of the service binary (only the platform: the migrator and the probe are built plain). EMPTY in every image except the harness's, which tests/harness/docker-compose.yml builds with the one tag that
# arms the fault points; no workflow and no other recipe may pass it (the fault-point gate in scripts/ fails on it), so a default build holds no armed code. An empty value is a build with no tags.
ARG GO_TAGS=""
RUN CGO_ENABLED=0 GOTOOLCHAIN=local go build -tags "${GO_TAGS}" -o /out/platform ./cmd/platform \
 && CGO_ENABLED=0 GOTOOLCHAIN=local go build -o /out/platform-migrate ./cmd/platform-migrate \
 && CGO_ENABLED=0 GOTOOLCHAIN=local go build -o /out/platform-probe ./cmd/platform-probe

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache ca-certificates && adduser -D -u 10003 platform
USER platform
COPY --from=build /out/platform /usr/local/bin/platform
COPY --from=build /out/platform-migrate /usr/local/bin/platform-migrate
COPY --from=build /out/platform-probe /usr/local/bin/platform-probe
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/platform"]
