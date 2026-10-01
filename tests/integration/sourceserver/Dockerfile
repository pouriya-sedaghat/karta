# syntax=docker/dockerfile:1.7
# Test-only image of the controlled HTTPS source (tests/integration/sourceserver).
# Built by `make test-integration`; never part of a deployment. Base images
# are pinned to the same digests as deploy/Dockerfile.
ARG GO_IMAGE=golang:1.27-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY tests/integration/sourceserver/ tests/integration/sourceserver/
RUN CGO_ENABLED=0 go build -trimpath -o /out/sourceserver ./tests/integration/sourceserver

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/sourceserver /usr/local/bin/sourceserver
USER 65532:65532
EXPOSE 8443 8444
ENTRYPOINT ["/usr/local/bin/sourceserver"]
