# HeurChain broker image. Pure-Go SQLite (modernc.org/sqlite): no cgo, so the
# runtime image is distroless static and runs as an unprivileged user.
# The build stage runs on the builder's own platform and cross-compiles for the
# target, so multi-arch images (linux/amd64, linux/arm64) need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.23-bookworm AS build
ARG COMMIT=unknown
ARG TARGETOS=linux
ARG TARGETARCH
ENV CGO_ENABLED=0
WORKDIR /src/broker
COPY broker/go.mod broker/go.sum ./
RUN go mod download
COPY broker/ ./
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.brokerCommit=${COMMIT}" -o /out/heurchain-broker . \
    && mkdir -p /out/data/vault

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/peterjohannmedina/heurchain"
LABEL org.opencontainers.image.description="HeurChain self-hosted memory broker"
LABEL org.opencontainers.image.licenses="MIT"
COPY --from=build --chown=65532:65532 /out/data /var/lib/heurchain
COPY --from=build /out/heurchain-broker /usr/local/bin/heurchain-broker
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/heurchain-broker"]
CMD ["-config", "/etc/heurchain/broker.yaml"]
