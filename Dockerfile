# Build stage
FROM golang:1.22-alpine AS builder

# CGO is required for go-sqlite3
RUN apk add --no-cache gcc musl-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-s -w" -o /bin/costblame ./cmd/costblame

# Runtime stage — minimal Alpine image
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app

COPY --from=builder /bin/costblame /usr/local/bin/costblame

VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["costblame"]
CMD ["serve"]
