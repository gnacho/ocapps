#!/usr/bin/env bash
# migrate.sh - 3-services data migration -> unified ocapps (SPEC §5.4).
#
# Principles: COPY, never move while hot; verification with
# PRAGMA integrity_check; rollback without loss (the source data stays
# intact). Idempotent: it can be re-run, every step detects the state
# and is safe to repeat. --dry-run prints what it would do without
# touching anything.
#
# Usage (as root on the production host):
#   sudo deploy/migrate.sh --dry-run     # rehearsal: prints only
#   sudo deploy/migrate.sh               # real migration
#   sudo deploy/migrate.sh --force-redeploy
#       deliberate redeploy WITH the new service active: it stops it,
#       copies the databases on top and restarts it. Without this flag,
#       if the new unit is active the script ABORTS (copying over live
#       databases would corrupt them).
#
# Rollback: see deploy/README.md (the old units are only DISABLED, not
# deleted, and /var/lib/ocnews|ocnotes|ocphotos stay intact).

set -euo pipefail

# ══════════════════════════════════════════════════════════════════════
# Parametrizable SOURCE paths (adjust to your real deployment if it
# differs; SPEC Annex B.2: confirm the production paths before running).
# Also overridable through the environment: OCNEWS_DIR=... ./migrate.sh
# ══════════════════════════════════════════════════════════════════════
OCNEWS_DIR="${OCNEWS_DIR:-/var/lib/ocnews}"
OCNOTES_DIR="${OCNOTES_DIR:-/var/lib/ocnotes}"
OCPHOTOS_DIR="${OCPHOTOS_DIR:-/var/lib/ocphotos}"

# Destination: it must match the new service's OCAPPS_DATA_DIR.
OCAPPS_DATA_DIR="${OCAPPS_DATA_DIR:-/var/lib/ocapps}"

# User and group of the new service (they must match the systemd unit).
OCAPPS_USER="${OCAPPS_USER:-ocapps}"
OCAPPS_GROUP="${OCAPPS_GROUP:-ocapps}"

# Old and new systemd units.
OLD_UNITS="${OLD_UNITS:-ocnews ocnotes ocphotos}"
NEW_UNIT="${NEW_UNIT:-opencloud-apps}"

# Address of the new listener for the final smoke checks.
OCAPPS_ADDR="${OCAPPS_ADDR:-127.0.0.1:8077}"

# user_version values expected after the migration (SPEC §5.3/§5.4):
#   news arrives with its live schema (18), notes the same (2), and
#   photos goes from unversioned (0) to the baseline (1) on the first
#   ocapps start.
EXPECTED_NEWS_UV=18
EXPECTED_NOTES_UV=2
EXPECTED_PHOTOS_UV=1

DRY_RUN=0
FORCE_REDEPLOY=0
for arg in "$@"; do
	case "$arg" in
		--dry-run) DRY_RUN=1 ;;
		--force-redeploy) FORCE_REDEPLOY=1 ;;
		*)
			printf 'XX  ERROR: unknown flag: %s (usage: migrate.sh [--dry-run] [--force-redeploy])\n' "$arg" >&2
			exit 2
			;;
	esac
done

# ── helpers ─────────────────────────────────────────────────────────────
log()  { printf '==> %s\n' "$*"; }
warn() { printf '!!  %s\n' "$*" >&2; }
die()  { printf 'XX  ERROR: %s\n' "$*" >&2; exit 1; }

# run: runs (or prints, in dry-run) a command without capturing output.
run() {
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '+ %s\n' "$*"
	else
		"$@"
	fi
}

# have_cmd: 0 if the command exists in PATH.
have_cmd() { command -v "$1" >/dev/null 2>&1; }

# integrity_check <db>: aborts if the database is not sound. In dry-run it
# only checks existence and readability if the file exists.
integrity_check() {
	db="$1"
	if [ ! -f "$db" ]; then
		if [ "$DRY_RUN" -eq 1 ]; then
			warn "database not found (the real run would abort here): $db"
			return 0
		fi
		die "database not found: $db"
	fi
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '+ sqlite3 %s "PRAGMA integrity_check;"  # expected: ok\n' "$db"
		return 0
	fi
	out="$(sqlite3 "$db" 'PRAGMA integrity_check;')" \
		|| die "sqlite3 failed on $db"
	[ "$out" = "ok" ] \
		|| die "integrity_check of $db is NOT ok: $out"
	log "integrity_check ok: $db"
}

# checkpoint <db>: flushes the WAL into the main file (TRUNCATE) so the copy
# is self-contained even if -wal/-shm files were left behind (SPEC §5.4, note).
checkpoint() {
	db="$1"
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '+ sqlite3 %s "PRAGMA wal_checkpoint(TRUNCATE);"\n' "$db"
		return 0
	fi
	sqlite3 "$db" 'PRAGMA wal_checkpoint(TRUNCATE);' >/dev/null \
		|| die "wal_checkpoint failed on $db"
	log "wal_checkpoint(TRUNCATE) ok: $db"
}

# copy_db <src_dir> <db_name> <dst_dir>: copies the .db and its -wal/-shm if
# they exist (cp preserves; NEVER mv, the source stays intact for rollback).
copy_db() {
	src_dir="$1" name="$2" dst_dir="$3"
	f="$src_dir/$name"
	if [ ! -f "$f" ]; then
		if [ "$DRY_RUN" -eq 1 ]; then
			warn "database not found (the real run would abort here): $f"
			return 0
		fi
		die "database not found: $f"
	fi
	for suffix in "" "-wal" "-shm"; do
		if [ -f "$f$suffix" ]; then
			run cp -a "$f$suffix" "$dst_dir/"
			log "copied $f$suffix -> $dst_dir/"
		fi
	done
}

# copy_aux <src_dir> <dst_dir> <items...>: copies the per-module auxiliary
# data (SPEC §5.4 step 3). Missing items are reported and skipped (for
# instance a deployment with no imgcache), without breaking idempotency.
copy_aux() {
	src_dir="$1" dst_dir="$2"
	shift 2
	for item in "$@"; do
		if [ -e "$src_dir/$item" ]; then
			run cp -a "$src_dir/$item" "$dst_dir/"
			log "copied $src_dir/$item -> $dst_dir/"
		else
			warn "auxiliary item missing, skipped: $src_dir/$item"
		fi
	done
}

# log_user_version <db> <expected> <mode:strict|info>: records user_version
# after the copy for the audit (SPEC §5.4 step 5).
log_user_version() {
	db="$1" expected="$2" mode="$3"
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '+ sqlite3 %s "PRAGMA user_version;"  # expected: %s\n' "$db" "$expected"
		return 0
	fi
	actual="$(sqlite3 "$db" 'PRAGMA user_version;')"
	if [ "$actual" = "$expected" ]; then
		log "user_version of $db = $actual (expected $expected) ok"
	elif [ "$mode" = strict ]; then
		warn "user_version of $db = $actual, expected $expected."
		warn "  ocapps will apply the pending migrations on the first start;"
		warn "  check journalctl -u $NEW_UNIT after it starts."
	else
		log "user_version of $db = $actual (pre-baseline; ocapps will set $expected on the first start)"
	fi
}

# ── 0. Pre-flight ───────────────────────────────────────────────────────
log "Step 0/7: pre-flight"
[ "$DRY_RUN" -eq 1 ] && log "DRY-RUN MODE: nothing is executed, everything is printed."
for c in sqlite3 systemctl curl; do
	if ! have_cmd "$c"; then
		if [ "$DRY_RUN" -eq 1 ]; then
			warn "command not found in PATH (required by the real run): $c"
		else
			die "required command not found: $c"
		fi
	fi
done
if [ "$DRY_RUN" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
	die "run as root (systemctl, install -o and cp -a into $OCAPPS_DATA_DIR need it), or use --dry-run"
fi
# I4: if the new service is ACTIVE its databases are in use, and copying the
# old ones on top would corrupt them. By default it ABORTS. --force-redeploy
# is the explicit redeploy path: it stops the unit, copies, and step 6
# restarts it (enable --now).
if [ "$DRY_RUN" -eq 0 ] && systemctl is-active --quiet "$NEW_UNIT"; then
	if [ "$FORCE_REDEPLOY" -eq 1 ]; then
		warn "$NEW_UNIT is active and --force-redeploy was given: it gets stopped, the data copied on top and restarted in step 6."
		run systemctl stop "$NEW_UNIT"
		if systemctl is-active --quiet "$NEW_UNIT"; then
			die "$NEW_UNIT is still active after stop; fix that before copying databases"
		fi
		log "stopped for redeploy: $NEW_UNIT"
	else
		die "$NEW_UNIT is ACTIVE: copying the databases over the live ones would corrupt them.
  If the migration already ran, there is no need to repeat it.
  To deliberately redeploy on top: run again with --force-redeploy
  (stops the unit, copies and restarts); or stop the unit first:
  systemctl stop $NEW_UNIT"
	fi
fi

# ── 0b. Stop the old services (order does not matter, wait for inactive) ─
log "Step 0b/7: stopping the old services: $OLD_UNITS"
for unit in $OLD_UNITS; do
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '+ systemctl stop %s\n' "$unit"
	elif systemctl list-unit-files "$unit.service" >/dev/null 2>&1 \
		&& systemctl is-active --quiet "$unit"; then
		run systemctl stop "$unit"
		log "stopped: $unit"
	else
		log "already inactive or no unit: $unit (nothing to do)"
	fi
done
if [ "$DRY_RUN" -eq 0 ]; then
	for unit in $OLD_UNITS; do
		if systemctl is-active --quiet "$unit" 2>/dev/null; then
			die "$unit is still active after stop; fix that before copying databases"
		fi
	done
fi

# ── 1. Verify source + WAL checkpoint (before copying) ──────────────────
log "Step 1/7: verifying SOURCE databases (integrity_check) and flushing the WAL"
for db in "$OCNEWS_DIR/ocnews.db" "$OCNOTES_DIR/notes.db" "$OCPHOTOS_DIR/memories.db"; do
	integrity_check "$db"
	checkpoint "$db"
done

# ── 2. Copy the databases to the new layout (cp preserves, never mv) ─────
log "Step 2/7: creating $OCAPPS_DATA_DIR/{news,notes,photos} and copying the databases"
run install -d -m 700 -o "$OCAPPS_USER" -g "$OCAPPS_GROUP" \
	"$OCAPPS_DATA_DIR" "$OCAPPS_DATA_DIR/news" "$OCAPPS_DATA_DIR/notes" "$OCAPPS_DATA_DIR/photos"
copy_db "$OCNEWS_DIR" ocnews.db "$OCAPPS_DATA_DIR/news"
copy_db "$OCNOTES_DIR" notes.db "$OCAPPS_DATA_DIR/notes"
copy_db "$OCPHOTOS_DIR" memories.db "$OCAPPS_DATA_DIR/photos"

# ── 3. Copy the per-module auxiliary data (SPEC §5.1) ───────────────────
log "Step 3/7: copying auxiliary data (secrets, caches, attachments)"
copy_aux "$OCNEWS_DIR" "$OCAPPS_DATA_DIR/news" favicons imgcache imgsecret feedsecret
copy_aux "$OCNOTES_DIR" "$OCAPPS_DATA_DIR/notes" attachments imgcache imgsecret
copy_aux "$OCPHOTOS_DIR" "$OCAPPS_DATA_DIR/photos" thumbs mediasecret
# Recursive ownership of the copied tree (cp -a preserves the old owners).
run chown -R "$OCAPPS_USER:$OCAPPS_GROUP" "$OCAPPS_DATA_DIR"

# ── 4. Verify destination ───────────────────────────────────────────────
log "Step 4/7: verifying DESTINATION databases (integrity_check)"
if [ "$DRY_RUN" -eq 1 ]; then
	printf '+ for db in %s/*/*.db; do %s; done  # ok x3\n' "$OCAPPS_DATA_DIR" \
		"sqlite3 \"\$db\" \"PRAGMA integrity_check;\""
else
	for db in "$OCAPPS_DATA_DIR"/*/*.db; do
		integrity_check "$db"
	done
fi

# ── 5. Record the expected versions (audit) ─────────────────────────────
log "Step 5/7: recording PRAGMA user_version (expected: news=$EXPECTED_NEWS_UV notes=$EXPECTED_NOTES_UV photos=$EXPECTED_PHOTOS_UV)"
log_user_version "$OCAPPS_DATA_DIR/news/ocnews.db" "$EXPECTED_NEWS_UV" strict
log_user_version "$OCAPPS_DATA_DIR/notes/notes.db" "$EXPECTED_NOTES_UV" strict
log_user_version "$OCAPPS_DATA_DIR/photos/memories.db" "$EXPECTED_PHOTOS_UV" info

# ── 6. systemd: disable the old ones (do NOT delete), enable and start ───
log "Step 6/7: systemd - disable the old ones (no deletion), enable and start $NEW_UNIT"
for unit in $OLD_UNITS; do
	run systemctl disable "$unit" || warn "could not disable $unit (already disabled, or no unit?)"
done
run systemctl enable --now "$NEW_UNIT"
if [ "$DRY_RUN" -eq 0 ]; then
	systemctl is-active --quiet "$NEW_UNIT" \
		|| die "$NEW_UNIT did not become active; check: journalctl -u $NEW_UNIT -n 100"
	log "$NEW_UNIT active."
fi

# ── 7. Smoke checks over localhost (full checklist: SPEC §9.4) ──────────
log "Step 7/7: smoke checks over localhost ($OCAPPS_ADDR)"
if [ "$DRY_RUN" -eq 1 ]; then
	printf '+ curl -s http://%s/healthz   # all 3 modules "ok"\n' "$OCAPPS_ADDR"
	printf '+ curl -s -o /dev/null -w %s http://%s/readyz   # 200\n' "'%{http_code}'" "$OCAPPS_ADDR"
else
	log "healthz: $(curl -fsS "http://$OCAPPS_ADDR/healthz")"
	code="$(curl -s -o /dev/null -w '%{http_code}' "http://$OCAPPS_ADDR/readyz")"
	if [ "$code" = 200 ]; then
		log "readyz: 200 ok"
	else
		warn "readyz returned $code: some module is failed (policy D3)."
		warn "  Check journalctl -u $NEW_UNIT and repeat the deploy/README.md checklist, Verification section."
	fi
fi

if [ "$DRY_RUN" -eq 1 ]; then
	log "DRY-RUN COMPLETE: nothing was modified."
else
	log "MIGRATION COMPLETE."
fi
cat <<EOF

Next steps:
  1. Full verification checklist (SPEC §9.4): see deploy/README.md.
  2. Proxy change: /ocphotos-api/ becomes routed WITHOUT strip
     (deploy/proxy/). Coordinate it with this cutover if you have not
     applied it yet.
  3. Rollback (window: as long as the data written into the new
     databases does not matter): systemctl stop $NEW_UNIT && systemctl enable --now $OLD_UNITS
     - the source data in $OCNEWS_DIR, $OCNOTES_DIR and $OCPHOTOS_DIR is
     intact (it was only read and copied). Details in deploy/README.md.
  4. After >=7 days of stability: archive the old directories and
     remove the $OLD_UNITS units (today they are only disabled).
EOF
