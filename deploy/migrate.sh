#!/usr/bin/env bash
# migrate.sh — migración de datos 3 servicios → ocapps unificado (SPEC §5.4).
#
# Principios: COPIA, nunca mover en caliente; verificación con
# PRAGMA integrity_check; rollback sin pérdida (los datos origen quedan
# intactos). Idempotente: puede re-ejecutarse; cada paso detecta el estado
# y es seguro repetirlo. --dry-run imprime lo que haría sin tocar nada.
#
# Uso (como root en el host de producción):
#   sudo deploy/migrate.sh --dry-run     # ensayo: solo imprime
#   sudo deploy/migrate.sh               # migración real
#
# Rollback: ver deploy/README.md (los units viejos solo se DESHABILITAN,
# no se borran, y /var/lib/ocnews|ocnotes|ocphotos quedan intactos).

set -euo pipefail

# ══════════════════════════════════════════════════════════════════════
# Rutas de ORIGEN parametrizables (ajusta a tu despliegue real si difiere;
# SPEC Anexo B.2: confirmar los paths de producción antes de ejecutar).
# También sobreescribibles por entorno: OCNEWS_DIR=... ./migrate.sh
# ══════════════════════════════════════════════════════════════════════
OCNEWS_DIR="${OCNEWS_DIR:-/var/lib/ocnews}"
OCNOTES_DIR="${OCNOTES_DIR:-/var/lib/ocnotes}"
OCPHOTOS_DIR="${OCPHOTOS_DIR:-/var/lib/ocphotos}"

# Destino: debe coincidir con OCAPPS_DATA_DIR del servicio nuevo.
OCAPPS_DATA_DIR="${OCAPPS_DATA_DIR:-/var/lib/ocapps}"

# Usuario/grupo del servicio nuevo (debe coincidir con el unit systemd).
OCAPPS_USER="${OCAPPS_USER:-ocapps}"
OCAPPS_GROUP="${OCAPPS_GROUP:-ocapps}"

# Units systemd viejos y nuevo.
OLD_UNITS="${OLD_UNITS:-ocnews ocnotes ocphotos}"
NEW_UNIT="${NEW_UNIT:-opencloud-apps}"

# Dirección del listener nuevo para los smoke checks finales.
OCAPPS_ADDR="${OCAPPS_ADDR:-127.0.0.1:8096}"

# user_version esperados tras la migración (SPEC §5.3/§5.4):
#   news llega con su esquema vivo (18), notes igual (2) y photos pasa de
#   sin versionar (0) al baseline (1) en el primer arranque de ocapps.
EXPECTED_NEWS_UV=18
EXPECTED_NOTES_UV=2
EXPECTED_PHOTOS_UV=1

DRY_RUN=0
[ "${1:-}" = "--dry-run" ] && DRY_RUN=1

# ── helpers ─────────────────────────────────────────────────────────────
log()  { printf '==> %s\n' "$*"; }
warn() { printf '!!  %s\n' "$*" >&2; }
die()  { printf 'XX  ERROR: %s\n' "$*" >&2; exit 1; }

# run: ejecuta (o imprime en dry-run) un comando sin captura.
run() {
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '+ %s\n' "$*"
	else
		"$@"
	fi
}

# have_cmd: 0 si el comando existe en PATH.
have_cmd() { command -v "$1" >/dev/null 2>&1; }

# integrity_check <db>: aborta si la BD no está íntegra. En dry-run solo
# comprueba existencia/legibilidad si el fichero existe.
integrity_check() {
	db="$1"
	if [ ! -f "$db" ]; then
		if [ "$DRY_RUN" -eq 1 ]; then
			warn "BD no encontrada (se abortaría aquí en la ejecución real): $db"
			return 0
		fi
		die "BD no encontrada: $db"
	fi
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '+ sqlite3 %s "PRAGMA integrity_check;"  # esperado: ok\n' "$db"
		return 0
	fi
	out="$(sqlite3 "$db" 'PRAGMA integrity_check;')" \
		|| die "sqlite3 falló sobre $db"
	[ "$out" = "ok" ] \
		|| die "integrity_check de $db NO es ok: $out"
	log "integrity_check ok: $db"
}

# checkpoint <db>: vuelca el WAL al fichero principal (TRUNCATE) para que la
# copia sea auto-contenida aunque quedaran -wal/-shm (SPEC §5.4, nota).
checkpoint() {
	db="$1"
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '+ sqlite3 %s "PRAGMA wal_checkpoint(TRUNCATE);"\n' "$db"
		return 0
	fi
	sqlite3 "$db" 'PRAGMA wal_checkpoint(TRUNCATE);' >/dev/null \
		|| die "wal_checkpoint falló sobre $db"
	log "wal_checkpoint(TRUNCATE) ok: $db"
}

# copy_db <src_dir> <db_name> <dst_dir>: copia el .db y sus -wal/-shm si
# existen (cp preserva; NUNCA mv — el origen queda intacto para rollback).
copy_db() {
	src_dir="$1" name="$2" dst_dir="$3"
	f="$src_dir/$name"
	if [ ! -f "$f" ]; then
		if [ "$DRY_RUN" -eq 1 ]; then
			warn "BD no encontrada (se abortaría aquí en la ejecución real): $f"
			return 0
		fi
		die "BD no encontrada: $f"
	fi
	for suffix in "" "-wal" "-shm"; do
		if [ -f "$f$suffix" ]; then
			run cp -a "$f$suffix" "$dst_dir/"
			log "copiado $f$suffix → $dst_dir/"
		fi
	done
}

# copy_aux <src_dir> <dst_dir> <items...>: copia datos auxiliares por módulo
# (SPEC §5.4 paso 3). Los items ausentes se avisan y se omiten (p. ej. un
# despliegue sin imgcache), sin romper la idempotencia.
copy_aux() {
	src_dir="$1" dst_dir="$2"
	shift 2
	for item in "$@"; do
		if [ -e "$src_dir/$item" ]; then
			run cp -a "$src_dir/$item" "$dst_dir/"
			log "copiado $src_dir/$item → $dst_dir/"
		else
			warn "auxiliar ausente, se omite: $src_dir/$item"
		fi
	done
}

# log_user_version <db> <esperado> <modo:strict|info>: registra para
# auditoría el user_version tras la copia (SPEC §5.4 paso 5).
log_user_version() {
	db="$1" expected="$2" mode="$3"
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '+ sqlite3 %s "PRAGMA user_version;"  # esperado: %s\n' "$db" "$expected"
		return 0
	fi
	actual="$(sqlite3 "$db" 'PRAGMA user_version;')"
	if [ "$actual" = "$expected" ]; then
		log "user_version de $db = $actual (esperado $expected) ✓"
	elif [ "$mode" = strict ]; then
		warn "user_version de $db = $actual, esperado $expected."
		warn "  ocapps aplicará las migraciones pendientes en el primer arranque;"
		warn "  revisa journalctl -u $NEW_UNIT tras el arranque."
	else
		log "user_version de $db = $actual (pre-baseline; ocapps fijará $expected en el primer arranque)"
	fi
}

# ── 0. Pre-vuelo ─────────────────────────────────────────────────────────
log "Paso 0/7: pre-vuelo"
[ "$DRY_RUN" -eq 1 ] && log "MODO DRY-RUN: no se ejecuta nada, solo se imprime."
for c in sqlite3 systemctl curl; do
	if ! have_cmd "$c"; then
		if [ "$DRY_RUN" -eq 1 ]; then
			warn "comando no encontrado en PATH (necesario en la ejecución real): $c"
		else
			die "comando requerido no encontrado: $c"
		fi
	fi
done
if [ "$DRY_RUN" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
	die "ejecuta como root (systemctl, install -o y cp -a a $OCAPPS_DATA_DIR lo requieren), o usa --dry-run"
fi
# Idempotencia: si las BDs ya están en destino y el servicio nuevo activo,
# la migración probablemente ya se hizo; seguimos (cada paso es re-seguro).
if [ "$DRY_RUN" -eq 0 ] && systemctl is-active --quiet "$NEW_UNIT"; then
	warn "$NEW_UNIT ya está activo: la migración parece ya aplicada."
	warn "  Los pasos siguientes son idempotentes, pero plantéate abortar (Ctrl-C) si no es intencionado."
	sleep 5
fi

# ── 0b. Parar servicios viejos (orden indiferente; esperar a inactive) ───
log "Paso 0b/7: parando servicios viejos: $OLD_UNITS"
for unit in $OLD_UNITS; do
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '+ systemctl stop %s\n' "$unit"
	elif systemctl list-unit-files "$unit.service" >/dev/null 2>&1 \
		&& systemctl is-active --quiet "$unit"; then
		run systemctl stop "$unit"
		log "parado: $unit"
	else
		log "ya inactivo o sin unit: $unit (nada que hacer)"
	fi
done
if [ "$DRY_RUN" -eq 0 ]; then
	for unit in $OLD_UNITS; do
		if systemctl is-active --quiet "$unit" 2>/dev/null; then
			die "$unit sigue activo tras stop; resuélvelo antes de copiar BDs"
		fi
	done
fi

# ── 1. Verificar origen + checkpoint WAL (antes de copiar) ───────────────
log "Paso 1/7: verificando BDs de ORIGEN (integrity_check) y volcando WAL"
for db in "$OCNEWS_DIR/ocnews.db" "$OCNOTES_DIR/notes.db" "$OCPHOTOS_DIR/memories.db"; do
	integrity_check "$db"
	checkpoint "$db"
done

# ── 2. Copiar BDs al nuevo layout (cp preserva; nunca mv) ────────────────
log "Paso 2/7: creando $OCAPPS_DATA_DIR/{news,notes,photos} y copiando BDs"
run install -d -m 700 -o "$OCAPPS_USER" -g "$OCAPPS_GROUP" \
	"$OCAPPS_DATA_DIR" "$OCAPPS_DATA_DIR/news" "$OCAPPS_DATA_DIR/notes" "$OCAPPS_DATA_DIR/photos"
copy_db "$OCNEWS_DIR" ocnews.db "$OCAPPS_DATA_DIR/news"
copy_db "$OCNOTES_DIR" notes.db "$OCAPPS_DATA_DIR/notes"
copy_db "$OCPHOTOS_DIR" memories.db "$OCAPPS_DATA_DIR/photos"

# ── 3. Copiar datos auxiliares por módulo (SPEC §5.1) ────────────────────
log "Paso 3/7: copiando datos auxiliares (secretos, cachés, adjuntos)"
copy_aux "$OCNEWS_DIR" "$OCAPPS_DATA_DIR/news" favicons imgcache imgsecret feedsecret
copy_aux "$OCNOTES_DIR" "$OCAPPS_DATA_DIR/notes" attachments imgcache imgsecret
copy_aux "$OCPHOTOS_DIR" "$OCAPPS_DATA_DIR/photos" thumbs hls mediasecret
# Propiedad recursiva del árbol copiado (cp -a preserva los owners viejos).
run chown -R "$OCAPPS_USER:$OCAPPS_GROUP" "$OCAPPS_DATA_DIR"

# ── 4. Verificar destino ──────────────────────────────────────────────────
log "Paso 4/7: verificando BDs de DESTINO (integrity_check)"
if [ "$DRY_RUN" -eq 1 ]; then
	printf '+ for db in %s/*/*.db; do %s; done  # ok x3\n' "$OCAPPS_DATA_DIR" \
		"sqlite3 \"\$db\" \"PRAGMA integrity_check;\""
else
	for db in "$OCAPPS_DATA_DIR"/*/*.db; do
		integrity_check "$db"
	done
fi

# ── 5. Registrar versiones esperadas (auditoría) ──────────────────────────
log "Paso 5/7: registrando PRAGMA user_version (esperados: news=$EXPECTED_NEWS_UV notes=$EXPECTED_NOTES_UV photos=$EXPECTED_PHOTOS_UV)"
log_user_version "$OCAPPS_DATA_DIR/news/ocnews.db" "$EXPECTED_NEWS_UV" strict
log_user_version "$OCAPPS_DATA_DIR/notes/notes.db" "$EXPECTED_NOTES_UV" strict
log_user_version "$OCAPPS_DATA_DIR/photos/memories.db" "$EXPECTED_PHOTOS_UV" info

# ── 6. systemd: disable viejos (NO borrar), enable+start unificado ────────
log "Paso 6/7: systemd — disable viejos (sin borrar), enable+start $NEW_UNIT"
for unit in $OLD_UNITS; do
	run systemctl disable "$unit" || warn "no se pudo deshabilitar $unit (¿ya deshabilitado o sin unit?)"
done
run systemctl enable --now "$NEW_UNIT"
if [ "$DRY_RUN" -eq 0 ]; then
	systemctl is-active --quiet "$NEW_UNIT" \
		|| die "$NEW_UNIT no quedó activo; revisa: journalctl -u $NEW_UNIT -n 100"
	log "$NEW_UNIT activo."
fi

# ── 7. Smoke checks por localhost (checklist completo: SPEC §9.4) ─────────
log "Paso 7/7: smoke checks por localhost ($OCAPPS_ADDR)"
if [ "$DRY_RUN" -eq 1 ]; then
	printf '+ curl -s http://%s/healthz   # los 3 módulos "ok"\n' "$OCAPPS_ADDR"
	printf '+ curl -s -o /dev/null -w %s http://%s/readyz   # 200\n' "'%{http_code}'" "$OCAPPS_ADDR"
else
	log "healthz: $(curl -fsS "http://$OCAPPS_ADDR/healthz")"
	code="$(curl -s -o /dev/null -w '%{http_code}' "http://$OCAPPS_ADDR/readyz")"
	if [ "$code" = 200 ]; then
		log "readyz: 200 ✓"
	else
		warn "readyz devolvió $code: algún módulo está failed (política D3)."
		warn "  Revisa journalctl -u $NEW_UNIT y repite el checklist de deploy/README.md §Verificación."
	fi
fi

if [ "$DRY_RUN" -eq 1 ]; then
	log "DRY-RUN COMPLETADO: no se ha modificado nada."
else
	log "MIGRACIÓN COMPLETADA."
fi
cat <<EOF

Siguientes pasos:
  1. Checklist de verificación completo (SPEC §9.4): ver deploy/README.md.
  2. Cambio de proxy: /ocphotos-api/ pasa a enrutarse SIN strip
     (deploy/proxy/). Coordínalo con este corte si no lo aplicaste ya.
  3. Rollback (ventana: mientras no importen los datos escritos en las
     BDs nuevas): systemctl stop $NEW_UNIT && systemctl enable --now $OLD_UNITS
     — los datos origen en $OCNEWS_DIR, $OCNOTES_DIR y $OCPHOTOS_DIR están
     intactos (solo se leyeron/copiaron). Detalles en deploy/README.md.
  4. Tras >=7 días de estabilidad: archivar los directorios viejos y
     eliminar los units $OLD_UNITS (hoy solo deshabilitados).
EOF
