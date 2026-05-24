FROM golang:1.26-alpine AS builder
RUN apk add --no-cache git
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /crosslink ./cmd/server

FROM node:22-alpine AS web-builder
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ .
RUN npm run build

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
RUN adduser -D -H crosslink
COPY --from=builder /crosslink /usr/local/bin/crosslink
COPY --from=web-builder /app/web/dist /app/web/dist
COPY configs/config.example.yaml /app/configs/config.example.yaml
COPY configs/providers.example.yaml /app/configs/providers.example.yaml
COPY migrations/ /app/migrations/
WORKDIR /app
USER crosslink
EXPOSE 8080
ENTRYPOINT ["crosslink"]
