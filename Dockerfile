# --- bild stressserver and stresstester then put outputs in a directory
# --- then  Run this file from inside the directory

# --- Stage 1: Build server Go binary ---
FROM  golang:1.27.1-alpine AS builder

WORKDIR /

COPY . ./



# --- Stage 2: Create the runtime image ---
# Match the alpine version used in the golang build stage
FROM  debian:bookworm-slim 

WORKDIR /


# Copy the IBM MQ AMD64 libraries to the final stage
COPY --from=builder ./mqm/. ./opt/mqm/.
ENV LD_LIBRARY_PATH="/opt/mqm/lib64" \
CGO_CFLAGS="-I/opt/mqm/inc" \
    CGO_LDFLAGS="-L/opt/mqm/lib64 -Wl,-rpath,/opt/mqm/lib64"

# 1. Copy the compiled binary
COPY --from=builder /stressserver ./stresstesterserver

# FIXED: Replaced missing 'thttpd' package with 'lighttpd'
RUN apt-get update && apt-get install -y \
    lighttpd \
    libstdc++6 \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Create the html folder and copy your HTML file
RUN mkdir -p ./html
COPY --from=builder /stressserver.html ./html/index.html

COPY --from=builder /stresstester ./stresstester
COPY --from=builder /flowtemplates ./flowtemplates
COPY --from=builder /messagetemplates ./messagetemplates
COPY --from=builder /files ./files


EXPOSE 8080
EXPOSE 8181

# Clean up: Remove CMD entirely, handle both processes in ENTRYPOINT.
# 1. Start thttpd in the background using '&' (without -D so it goes to background)
# 2. Print your pre-run text
# 3. Use 'exec' to run your Go binary as the main process (PID 1)
ENTRYPOINT ["sh", "-c", "thttpd -p 8080 -d ./html & echo 'Performing pre-run tasks...' && exec /stresstesterserver"]