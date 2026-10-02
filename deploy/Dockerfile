# syntax=docker/dockerfile:1.7
#
# Karta images. Every base image is pinned by manifest-list digest.
#   target api       distroless, non-root: `karta serve` plus the demo assets
#                    (also `karta operator`, the operator API client, and
#                    `karta fetcher`, the opt-in online source poller)
#   target importer  Ubuntu 24.04 + osm2pgsql 1.11.0, non-root: `karta import`
#                    and `karta publisher` (entrypoint overridden in compose);
#                    also karta-load, the capacity measuring tool
#
# Optional build secret `build_ca`: a PEM bundle for networks that intercept
# TLS (corporate proxies). Without it the default trust stores are used.
#   docker build --secret id=build_ca,src=/path/ca.pem ...

ARG NODE_IMAGE=node:22-bookworm-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c
ARG GO_IMAGE=golang:1.27-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG IMPORTER_BASE=ubuntu:24.04@sha256:008173c23f95b170204355c12626cb5a965d779a7e1283b09e9cffbb1bf33ca3

FROM ${NODE_IMAGE} AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=secret,id=build_ca,required=false \
    if [ -s /run/secrets/build_ca ]; then export NODE_EXTRA_CA_CERTS=/run/secrets/build_ca; fi; \
    npm ci --ignore-scripts --no-audit --no-fund
COPY web/build.mjs web/index.html web/app.js web/app.css ./
RUN node build.mjs

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=build_ca,required=false \
    if [ -s /run/secrets/build_ca ]; then \
      cat /etc/ssl/certs/ca-certificates.crt /run/secrets/build_ca > /tmp/ca.pem; export SSL_CERT_FILE=/tmp/ca.pem; \
    fi; \
    go mod download && go mod verify
COPY cmd/ cmd/
COPY internal/ internal/
COPY openapi/ openapi/
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/karta ./cmd/karta \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/karta-load ./cmd/karta-load \
 && mkdir -p /out/online

FROM ${RUNTIME_IMAGE} AS api
COPY --from=build /out/karta /usr/local/bin/karta
COPY --from=web /src/web/dist /opt/karta/web
# The fetcher's outbox (a named volume takes this owner when first mounted).
COPY --from=build --chown=65532:65532 /out/online /var/lib/karta/online
ENV KARTA_WEB_DIR=/opt/karta/web
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=4s --start-period=10s --retries=3 CMD ["/usr/local/bin/karta", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/karta"]
CMD ["serve"]

FROM ${IMPORTER_BASE} AS importer
RUN apt-get update \
 && apt-get install -y --no-install-recommends osm2pgsql=1.11.0+ds-1 \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin karta \
 && install -d -o 10001 -g 10001 -m 0700 /var/lib/karta/staging \
 && install -d -o root -g root -m 0755 /data/inbox \
 && install -d -o 65532 -g 65532 -m 0755 /data/online
COPY --from=build /out/karta /out/karta-load /usr/local/bin/
USER 10001:10001
WORKDIR /tmp
ENTRYPOINT ["/usr/local/bin/karta", "import"]
