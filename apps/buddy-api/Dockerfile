FROM golang:1.21-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o buddy-api .

FROM scratch

COPY --from=builder /app/buddy-api /app/buddy-api

EXPOSE 8080

ENV PORT=8080
ENV OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317
ENV RANDOM_500_RATE=0.0
ENV LATENCY_MS=0
ENV READINESS_FAIL_DURATION_SECONDS=0
ENV CRASH_LOOP_AFTER_N_REQUESTS=0
ENV CPU_BURN_SECONDS=0

ENTRYPOINT ["/app/buddy-api"]
