# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY *.go ./
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/bibliofil-mcp .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bibliofil-mcp /bibliofil-mcp
ENV PORT=8080 MCP_TRANSPORT=http
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["/bibliofil-mcp", "-healthcheck"]
ENTRYPOINT ["/bibliofil-mcp"]
