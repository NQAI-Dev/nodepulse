FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o nodepulse-server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o nodepulse-agent ./cmd/agent

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/nodepulse-server /app/
COPY --from=builder /app/nodepulse-agent /app/
COPY --from=builder /app/web/public /app/web/public
EXPOSE 8080
ENTRYPOINT ["/app/nodepulse-server", "-addr", ":8080"]
