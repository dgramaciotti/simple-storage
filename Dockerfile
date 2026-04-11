# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy mod files
COPY go.mod ./
RUN go mod download

# Copy source code
COPY main.go ./

# Build binary
RUN CGO_ENABLED=0 GOOS=linux go build -o storage-service main.go

# Final stage
FROM alpine:latest

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/storage-service .

# Create storage directory
RUN mkdir -p /data

# Set environment variables
ENV STORAGE_DIR=/data
ENV PORT=8080
ENV MAX_BUCKET_SIZE_GB=1
# Note: STORAGE_API_KEY must be provided at runtime

EXPOSE 8080

ENTRYPOINT ["./storage-service"]
