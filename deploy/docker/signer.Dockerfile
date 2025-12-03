FROM golang:1.24-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/bin ./cmd/signer

FROM alpine:3.19
WORKDIR /app
COPY --from=builder /app/bin .
CMD ["./bin"] 
