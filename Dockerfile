# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY *.go ./
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/bibliofil-mcp .

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/Olsenius/bibliofil-mcp"
COPY --from=build /out/bibliofil-mcp /bibliofil-mcp
ENV PORT=8080 MCP_TRANSPORT=http
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["/bibliofil-mcp", "-healthcheck"]
ENTRYPOINT ["/bibliofil-mcp"]
