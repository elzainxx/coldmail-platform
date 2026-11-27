# The platform image: the service, its migrator and the harness probe, as three static binaries, and the built web app, in one small image. Built by the harness (tests/harness/run.sh)
# and, later, by the deployment. Base images are pinned by digest (the Go and Alpine ones are the digests the three modules pin); CI builds with --pull --no-cache.
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

# The web app (Phase 6): npm ci installs exactly what web/package-lock.json says (nothing is resolved at build time), npm run build type-checks and builds it with Vite into /web/dist. Only that directory
# reaches the image. The base is the index of node:24-alpine (the major web/.node-version names), pinned by digest like every other base; .dockerignore keeps a developer's own node_modules and dist out of the
# context, because a copied node_modules would replace the one npm ci just made (and hold another platform's binaries).
FROM node:24-alpine@sha256:116b800c3754b440ee0aa3ed2f20f5bf3d99a554bfd9adeea09ea8017f6e85f5 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache ca-certificates && adduser -D -u 10003 platform
USER platform
COPY --from=build /out/platform /usr/local/bin/platform
COPY --from=build /out/platform-migrate /usr/local/bin/platform-migrate
COPY --from=build /out/platform-probe /usr/local/bin/platform-probe
# The built web app, at the fixed path an operator gives the service: PLATFORM_WEB_DIR=/srv/web (Docs/install.md). It is deliberately NOT set here: the dial's default is "the app is not served" (Docs/config-reference.md),
# and a set dial requires the two public Supabase dials, so an image that set it would refuse to start in every deployment that has not given them (the harness's included).
COPY --from=web /web/dist /srv/web
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/platform"]
