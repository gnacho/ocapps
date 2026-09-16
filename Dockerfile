# ocapps — backend unificado (news + notes + photos) para OpenCloud.
# Build:  docker build -t ocapps .
#         (opcional: --build-arg VERSION=$(git describe --tags))
# Run:    docker run -e OCAPPS_OPENCLOUD_URL=... -v ocapps-data:/var/lib/ocapps ocapps

# --- etapa 1: build (Go puro, modernc.org/sqlite → binario estático sin CGO) ---
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

# --- etapa 2: runtime ---
# ffmpeg: única dependencia de sistema (SPEC Q4/Q10 — pósters de vídeo y HLS
# del módulo photos). ca-certificates: TLS hacia el servidor OpenCloud.
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata ffmpeg \
    && adduser -D -u 10001 ocapps
COPY --from=build /ocapps /usr/local/bin/ocapps
ENV OCAPPS_LISTEN_ADDR=0.0.0.0:8096 \
    OCAPPS_DATA_DIR=/var/lib/ocapps
VOLUME /var/lib/ocapps
EXPOSE 8096
USER ocapps
ENTRYPOINT ["/usr/local/bin/ocapps"]
