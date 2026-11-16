# The platform harness's CONTROLLABLE SMTP sink (module 3's own sink, Mailpit, cannot reject, delay or throttle): a stdlib-only Go program (main.go, smtp.go, api.go,
# store.go) with a control API next to the SMTP port. See main.go for what it does and tests/harness/docker-compose.yml (service smtp-ctl) for how it is run.
#
# Build context = THIS directory. The program imports nothing outside the standard library, so the image gets its own one-line go.mod instead of the repository's: the build
# needs no module download and no network at all, and nothing of the platform's dependency graph can reach it. (The `*_test.go` files are copied and ignored by `go build`.)
# Both base images are the digests the platform and module 1 images pin (see tests/harness/m1/Dockerfile); CI builds with --pull --no-cache.
FROM golang:1.26-alpine@sha256:87c65a688975c0b1d4585d204874c45ca78354670e3a5f11368c5a492303d696 AS build
WORKDIR /src
COPY *.go ./
RUN printf 'module sink\n\ngo 1.26.0\n' > go.mod \
 && CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -o /out/sink .

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
# Non-root. The numeric user below is only the default: the harness runs the container as the user that owns module 3's dev key (compose `user:`), because that key is
# mode 600 and owned by whoever generated it (tools/own-secrets.sh leaves sink.key to the host user), and a different uid could not read it.
RUN adduser -D -u 10004 sink
USER sink
COPY --from=build /out/sink /usr/local/bin/sink
EXPOSE 1025 8025
ENTRYPOINT ["/usr/local/bin/sink"]
