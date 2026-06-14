# Build a CGO-free static binary for the target platform, then ship it on a
# minimal static base (includes CA certs for HTTPS, runs nonroot).
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH TARGETVARIANT VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/sentagent ./cmd/sentagent

# Runs as root: the agent must read the node's root-owned keyring + config.toml on
# the shared volume and use the Docker socket. The socket-proxy is the containment.
FROM gcr.io/distroless/static
COPY --from=build /out/sentagent /usr/local/bin/sentagent
ENTRYPOINT ["/usr/local/bin/sentagent"]
