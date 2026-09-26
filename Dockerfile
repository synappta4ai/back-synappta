# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /synapta .

# Runtime stage
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S app && adduser -S app -G app

WORKDIR /app

COPY --from=builder /synapta /app/synapta
COPY --from=builder /app/migrations /app/migrations

RUN mkdir -p /app/uploads /app/outputs && chown -R app:app /app
USER app

EXPOSE 8099

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:${PORT:-8099}/healthz || exit 1

ENTRYPOINT ["/app/synapta"]
