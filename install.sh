#!/bin/sh
# install.sh — instala el backend ocapps y sus extensiones web en OpenCloud.
#
#   curl -fsSL https://raw.githubusercontent.com/gnacho/ocapps/main/install.sh | sh -s -- --url https://cloud.example.com
#
# Opciones:
#   --url URL            URL pública de OpenCloud (obligatoria, salvo --dry-run)
#   --apps a,b,c         extensiones a instalar (default: news,notes,ocphotos)
#   --no-extensions      solo el backend
#   --admin-token TOK    activa el panel de extensiones (/ocapps-admin/)
#   --version vX.Y.Z     versión del binario (default: última release)
#   --dry-run            enseña lo que haría, sin tocar nada
#   --uninstall          para y elimina el servicio (conserva /var/lib/ocapps)
#   --purge              como --uninstall, y borra también los datos
#
# Requisitos: root, curl, tar, systemd. No instala dependencias del sistema.

set -eu

REPO="gnacho/ocapps"
APP_USER="ocapps"
BIN="/usr/local/bin/ocapps"
ETC="/etc/ocapps"
UNIT="/etc/systemd/system/opencloud-apps.service"
HELPER="/usr/local/sbin/ocapps-ext"
APPS_DIR="/etc/opencloud/web/assets/apps"
SERVICE="opencloud"
UPDATER_URL="https://raw.githubusercontent.com/gnacho/ocapps/main/deploy/extensions-updater.sh"

OC_URL=""
APPS="news,notes,ocphotos"
WITH_EXTENSIONS=1
ADMIN_TOKEN=""
VERSION=""
DRY_RUN=0
UNINSTALL=0
PURGE=0

usage() {
  sed -n '3,20p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-1}"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --url) shift; OC_URL="${1:-}" ;;
    --url=*) OC_URL="${1#*=}" ;;
    --apps) shift; APPS="${1:-}" ;;
    --apps=*) APPS="${1#*=}" ;;
    --no-extensions) WITH_EXTENSIONS=0 ;;
    --admin-token) shift; ADMIN_TOKEN="${1:-}" ;;
    --admin-token=*) ADMIN_TOKEN="${1#*=}" ;;
    --version) shift; VERSION="${1:-}" ;;
    --version=*) VERSION="${1#*=}" ;;
    --dry-run) DRY_RUN=1 ;;
    --uninstall) UNINSTALL=1 ;;
    --purge) UNINSTALL=1; PURGE=1 ;;
    -h|--help) usage 0 ;;
    *) echo "opción desconocida: $1" >&2; usage ;;
  esac
  shift
done

say() { printf '%s\n' "$*"; }
run() {
  if [ "$DRY_RUN" = 1 ]; then
    printf '  [dry-run] %s\n' "$*"
  else
    eval "$@"
  fi
}

need_root() {
  [ "$DRY_RUN" = 1 ] && return 0   # dry-run no toca el sistema
  [ "$(id -u)" = 0 ] || { echo "error: ejecuta como root" >&2; exit 1; }
}

need() { command -v "$1" >/dev/null 2>&1 || { echo "error: falta $1" >&2; exit 1; }; }

# ---------- desinstalar ----------
if [ "$UNINSTALL" = 1 ]; then
  need_root
  say "== parando y eliminando el servicio"
  run "systemctl disable --now opencloud-apps 2>/dev/null || true"
  run "rm -f '$UNIT'"
  run "systemctl daemon-reload"
  run "rm -f '$BIN'"
  say "  conservado: $ETC, /var/lib/ocapps y $HELPER"
  if [ "$PURGE" = 1 ]; then
    say "== --purge: borrando datos y configuración"
    run "rm -rf '$ETC' /var/lib/ocapps"
  fi
  say "hecho. Las extensiones desplegadas NO se tocan: quítalas de $APPS_DIR."
  exit 0
fi

# ---------- instalar ----------
need_root
need curl
need tar

[ -n "$OC_URL" ] || { echo "error: falta --url (URL pública de OpenCloud)" >&2; usage; }
case "$OC_URL" in http://*|https://*) ;; *) echo "error: --url debe empezar por http(s)://" >&2; exit 1 ;; esac
OC_URL="${OC_URL%/}"

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) echo "error: arquitectura no soportada: $arch" >&2; exit 1 ;;
esac

say "== ocapps en $OC_URL ($GOARCH)$([ "$DRY_RUN" = 1 ] && echo ' [dry-run]')"

# 1. versión y binario
if [ -z "$VERSION" ]; then
  api="https://api.github.com/repos/$REPO/releases/latest"
  VERSION=$(curl -fsSL "$api" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$VERSION" ] || { echo "error: no se pudo resolver la última versión" >&2; exit 1; }
fi
case "$VERSION" in v*) ;; *) VERSION="v$VERSION" ;; esac
plain="${VERSION#v}"
tarball="ocapps_${plain}_linux_${GOARCH}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"
say "   versión: $VERSION"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if [ "$DRY_RUN" = 0 ]; then
  curl -fsSL -o "$tmp/$tarball" "$base/$tarball"
  curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
  ( cd "$tmp" && grep " $tarball\$" checksums.txt | sha256sum -c - ) || {
    echo "error: el checksum no coincide" >&2; exit 1; }
  tar xzf "$tmp/$tarball" -C "$tmp"
fi

# 2. usuario, binario y entorno
say "== usuario y binario"
if ! id "$APP_USER" >/dev/null 2>&1; then
  run "useradd --system --user-group --home-dir /var/lib/ocapps --shell /usr/sbin/nologin $APP_USER"
fi
if [ "$DRY_RUN" = 0 ]; then
  install -m 755 -o root -g root "$tmp/ocapps" "$BIN"
else
  say "  [dry-run] install -m 755 ocapps $BIN"
fi

say "== entorno en $ETC/env"
run "install -d -m 750 -o root -g $APP_USER '$ETC'"
if [ -f "$ETC/env" ]; then
  say "  ya existe, no se toca (revisa OCAPPS_OPENCLOUD_URL a mano)"
  run "sed -i 's|^OCAPPS_OPENCLOUD_URL=.*|OCAPPS_OPENCLOUD_URL=$OC_URL|' '$ETC/env'"
else
  [ "$DRY_RUN" = 1 ] && say "  [dry-run] escribir $ETC/env"
  if [ "$DRY_RUN" = 0 ]; then
    cat > "$ETC/env" <<EOF
OCAPPS_OPENCLOUD_URL=$OC_URL
OCAPPS_LISTEN_ADDR=0.0.0.0:8096
OCAPPS_DATA_DIR=/var/lib/ocapps
OCAPPS_LOG_LEVEL=info
OCAPPS_AUTH_MODE=opencloud
OCAPPS_NEWS_PUBLIC_URL=$OC_URL/index.php/apps/news/api/v1-3
OCAPPS_NEWS_RETENTION_DAYS=90
OCAPPS_PHOTOS_SCAN_ROOT=Fotos
OCAPPS_PHOTOS_SCAN_EVERY=30m
EOF
    [ -n "$ADMIN_TOKEN" ] && printf 'OCAPPS_ADMIN_TOKEN=%s\n' "$ADMIN_TOKEN" >> "$ETC/env"
    chown "root:$APP_USER" "$ETC/env"
    chmod 640 "$ETC/env"
  fi
fi

# 3. unidad systemd
say "== servicio systemd"
if [ "$DRY_RUN" = 0 ]; then
  cat > "$UNIT" <<'EOF'
[Unit]
Description=OpenCloud Apps (news+notes+photos unified backend)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=ocapps
Group=ocapps
EnvironmentFile=/etc/ocapps/env
ExecStart=/usr/local/bin/ocapps
Restart=on-failure
RestartSec=3
StateDirectory=ocapps
StateDirectoryMode=0700
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/ocapps
NoNewPrivileges=false

[Install]
WantedBy=multi-user.target
EOF
fi
run "systemctl daemon-reload"
run "systemctl enable --now opencloud-apps"

# 4. extensiones
if [ "$WITH_EXTENSIONS" = 1 ] && [ -n "$APPS" ]; then
  say "== actualizador y extensiones ($APPS)"
  if [ ! -x "$HELPER" ]; then
    run "curl -fsSL -o '$HELPER' '$UPDATER_URL' && chmod 755 '$HELPER'"
  fi
  if [ "$DRY_RUN" = 0 ]; then
    [ -f "$ETC/extensions.conf" ] || printf '%s\n' \
      "news|gnacho/ocnews|^news-[0-9.]+\\.zip\$" \
      "notes|gnacho/ocnotes|^notes-[0-9.]+\\.zip\$" \
      "ocphotos|gnacho/ocphotos|^ocphotos-[0-9.]+\\.zip\$" > "$ETC/extensions.conf"
  fi
  old_ifs=$IFS; IFS=','
  for app in $APPS; do
    IFS=$old_ifs
    [ -n "$app" ] || continue
    say "   - $app"
    run "'$HELPER' install '$app'"
    IFS=','
  done
  IFS=$old_ifs
fi

say ""
say "== comprobación"
if [ "$DRY_RUN" = 1 ]; then
  say "  [dry-run] systemctl is-active opencloud-apps && curl -f http://127.0.0.1:8096/readyz"
else
  sleep 2
  systemctl is-active opencloud-apps || { echo "error: el servicio no arrancó (journalctl -u opencloud-apps)" >&2; exit 1; }
  curl -fsS -o /dev/null http://127.0.0.1:8096/readyz && say "  readyz OK"
  if [ -x "$HELPER" ]; then "$HELPER" list || true; fi
  say ""
  say "Falta el proxy: publica /ocphotos-api/, /index.php/apps/news/, /index.php/apps/notes/,"
  say "los dos /ocs/v2.php/cloud/* y /ocapps-admin/ hacia 127.0.0.1:8096."
fi
