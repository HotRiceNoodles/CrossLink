FROM golang:1.26-alpine AS builder
RUN apk add --no-cache git
# VERSION is passed by CI via --build-arg (e.g. --build-arg VERSION=v0.2.0);
# local builds without it fall back to "dev".
ARG VERSION=dev
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w \
	-X github.com/crosslink/internal/version.Version=${VERSION} \
	-X github.com/crosslink/internal/version.Commit=$(git rev-parse --short HEAD) \
	-X github.com/crosslink/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
	-o /crosslink ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
RUN adduser -D -H crosslink
COPY --from=builder /crosslink /usr/local/bin/crosslink
COPY deployments/docker/config.yaml /app/configs/config.yaml
COPY configs/providers.example.yaml /app/configs/providers.yaml
COPY migrations/ /app/migrations/
WORKDIR /app
USER crosslink
EXPOSE 8080
ENTRYPOINT ["crosslink"]
