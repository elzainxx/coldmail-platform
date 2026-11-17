# The platform harness's FIXTURE for module 1's real verifier node (DNS, the "what is my IP" HTTPS endpoint, the recipient's SMTP host, a control API): a stdlib-only Go
# program (main.go, state.go, dns.go, smtp.go, ipify.go, api.go, selfcheck.go). See main.go for what it does and tests/harness/docker-compose.m1node.yml (service fx) for how it is run.
#
# Build context = THIS directory, like tests/harness/sink: the program imports nothing outside the standard library, so the image gets its own one-line go.mod instead of the
# repository's, the build needs no module download and no network at all, and nothing of the platform's dependency graph can reach it. (The `*_test.go` files are copied and
# ignored by `go build`.) The fixture file (tests/harness/m1node/fixture.json) is NOT in the image: the harness mounts it read-only, so a change to it needs no rebuild.
# Both base images are the digests the sink and module 1 images pin; CI builds with --pull --no-cache.
FROM golang:1.26-alpine@sha256:87c65a688975c0b1d4585d204874c45ca78354670e3a5f11368c5a492303d696 AS build
WORKDIR /src
COPY *.go ./
RUN printf 'module fx\n\ngo 1.26.0\n' > go.mod \
 && CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -o /out/fx .

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
# Non-root. The numeric user below is only the default: the harness runs the container as the user that owns the run's TLS key (compose `user:`), because that key is
# mode 600 and a different uid could not read it. The ports below 1024 (53, 443) are bound by a non-root user because compose sets net.ipv4.ip_unprivileged_port_start=0
# for this container (a namespaced sysctl; no capability is granted).
RUN adduser -D -u 10005 fx
USER fx
COPY --from=build /out/fx /usr/local/bin/fx
EXPOSE 53/udp 53/tcp 443 2525 8025
ENTRYPOINT ["/usr/local/bin/fx"]
