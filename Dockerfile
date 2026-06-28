# Public-base build (GitHub Actions → ghcr.io). go.mod targets go 1.24.0;
# GOTOOLCHAIN=local avoids any toolchain download.
FROM golang:1.24-alpine AS build
ENV GOTOOLCHAIN=local
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/katalog-manager ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/katalog-manager /katalog-manager
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/katalog-manager"]
LABEL org.opencontainers.image.source="https://github.com/zaentrum/katalog-manager"
LABEL org.opencontainers.image.title="katalog-manager"
LABEL org.opencontainers.image.description="Catalog-management API (Go + GraphQL) for the zaentrum platform"
