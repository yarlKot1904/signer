FROM golang:1.24-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/bin ./cmd/uploader

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
RUN apk add --no-cache mailcap
WORKDIR /app
COPY --from=builder /app/bin .
COPY --from=builder /build/static ./static
CMD ["./bin"]