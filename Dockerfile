# ocapps - unified backend (news + notes + photos) for OpenCloud.
# Build:  docker build -t ocapps .
#         (optional: --build-arg VERSION=$(git describe --tags))
# Run:    docker run -e OCAPPS_OPENCLOUD_URL=... -v ocapps-data:/var/lib/ocapps ocapps

# --- stage 1: build (pure Go, modernc.org/sqlite, so a static binary with no CGO) ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /ocapps ./cmd/ocapps

# --- stage 2: runtime ---
# ffmpeg: the only system dependency (SPEC Q4/Q10, video posters of the photos
# module. HLS was removed in Q7, so ffmpeg is no longer used for streaming).
# ca-certificates: TLS towards the OpenCloud server.
FROM alpine:3.21
# The data dir is created here and handed to the service user: a named volume
# inherits the owner of the directory in the image, and without this it starts
# as root:root and the process (uid 10001) cannot write to it.
RUN apk add --no-cache ca-certificates tzdata ffmpeg \
    && adduser -D -u 10001 ocapps \
    && mkdir -p /var/lib/ocapps \
    && chown ocapps:ocapps /var/lib/ocapps
COPY --from=build /ocapps /usr/local/bin/ocapps
ENV OCAPPS_LISTEN_ADDR=0.0.0.0:8077 \
    OCAPPS_DATA_DIR=/var/lib/ocapps
VOLUME /var/lib/ocapps
EXPOSE 8077
USER ocapps
ENTRYPOINT ["/usr/local/bin/ocapps"]
