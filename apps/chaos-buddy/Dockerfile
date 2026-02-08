FROM golang:1.21-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o chaos-buddy .

FROM scratch

COPY --from=builder /app/chaos-buddy /app/chaos-buddy

EXPOSE 8082

ENV PORT=8082
ENV CHAOS_ENABLED=false
ENV KILL_INTERVAL_SECONDS=60

ENTRYPOINT ["/app/chaos-buddy"]
