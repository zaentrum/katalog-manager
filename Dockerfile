# syntax=docker/dockerfile:1.7
# Deploy build (GitLab CI / internal mirror). go.mod targets go 1.24.0, which the
# mirror's golang:1.24-alpine satisfies — GOTOOLCHAIN=local avoids any download.
FROM registry.nalet.cloud/infrastructure/library/golang:1.24-alpine AS build
ENV GOTOOLCHAIN=local
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/katalog-manager ./cmd/server

FROM registry.nalet.cloud/infrastructure/library/distroless/static-debian12:nonroot
COPY --from=build /out/katalog-manager /katalog-manager
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/katalog-manager"]
LABEL org.opencontainers.image.title="katalog-manager"
LABEL org.opencontainers.image.description="Catalog-management API (Go + GraphQL) for the zaentrum platform"
