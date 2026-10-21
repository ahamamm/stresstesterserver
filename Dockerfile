# --- Stage 1: Build the Go binary ---
FROM golang:1.27.1-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
# Uncomment if you have a go.sum file
# COPY go.sum ./ 
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /stresstesterserver-b .

# --- Stage 2: Create the runtime image ---
FROM alpine:latest  

WORKDIR /

# 1. Copy the compiled binary
COPY --from=builder /stresstesterserver-b ./stresstesterserver

# 2. Copy the static HTML files into the runner image
COPY --from=builder /app/stressserver.html ./web/index.html

EXPOSE 8080
EXPOSE 8181


ENTRYPOINT ["sh", "-c", "echo 'Performing pre-run tasks...' && exec /stresstesterserver"]