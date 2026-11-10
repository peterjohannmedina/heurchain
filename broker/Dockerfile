# Multi-stage build for the HeurChain broker.
# Pure-Go SQLite (modernc.org/sqlite) means no cgo, so the final image
# can use distroless static for minimal attack surface.

FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /heurchain-broker .

FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /heurchain-broker /usr/local/bin/heurchain-broker
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/heurchain-broker"]
CMD ["-config", "/etc/heurchain/broker.yaml"]
