#!/usr/bin/env bash
# Tests for the extension updater. No root, no network, no systemd: every path
# is redirected through the OCAPPS_EXT_* overrides and chown is stubbed, which
# is why the script can be sourced instead of executed.
#
# Run: bash deploy/extensions-updater_test.sh

set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

export OCAPPS_EXT_APPS_DIR="$work/apps"
export OCAPPS_EXT_BACKUP_DIR="$work/backups"
export OCAPPS_EXT_CONF="$work/extensions.conf"
export OCAPPS_EXT_APPS_YAML="$work/apps.yaml"
export QUIET=1
mkdir -p "$OCAPPS_EXT_APPS_DIR" "$OCAPPS_EXT_BACKUP_DIR"

# tests run as the invoking user, so ownership changes are meaningless
chown() { :; }

# shellcheck source=extensions-updater.sh
source "$here/extensions-updater.sh"

failures=0
ok() { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; failures=$((failures + 1)); }

expect() { # expect <description> <expected> <actual>
    if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (expected '$2', got '$3')"; fi
}

make_zip() { # make_zip <path> <version>
    python3 - "$1" "$2" <<'PY'
import json, sys, zipfile
out, version = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(out, "w") as z:
    z.writestr("demo/manifest.json", json.dumps({"version": version, "entrypoint": "js/app.mjs"}))
    z.writestr("demo/js/app.mjs", "console.log('demo')\n")
PY
}

# --- stamps ----------------------------------------------------------------
mkdir -p "$OCAPPS_EXT_APPS_DIR/demo"
write_stamp demo "release:demo-v1.0.0"
expect "stamp round trip" "release:demo-v1.0.0" "$(stamp_of demo)"

mkdir -p "$OCAPPS_EXT_APPS_DIR/nostamp"
expect "missing stamp reads as unknown" "unknown" "$(stamp_of nostamp)"

# writing into a directory that does not exist has to fail loudly instead of
# leaving the app without a stamp
if ( write_stamp missing-app release:demo-v1.0.0 ) 2>/dev/null; then
    bad "writing a stamp for a missing app fails"
else
    ok "writing a stamp for a missing app fails"
fi

# --- install_archive -------------------------------------------------------
# start from nothing so the first install has no previous directory to keep
rm -rf "${OCAPPS_EXT_APPS_DIR:?}/demo"
make_zip "$work/demo-100.zip" "1.0.0"
install_archive demo "$work/demo-100.zip" demo-v1.0.0
expect "install writes the stamp" "release:demo-v1.0.0" "$(stamp_of demo)"
expect "install keeps the manifest" "1.0.0" "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["version"])' "$OCAPPS_EXT_APPS_DIR/demo/manifest.json")"
expect "first install has nothing to back up" "" "$LAST_BACKUP"

make_zip "$work/demo-110.zip" "1.1.0"
install_archive demo "$work/demo-110.zip" demo-v1.1.0
expect "second install updates the stamp" "release:demo-v1.1.0" "$(stamp_of demo)"
if [ -n "$LAST_BACKUP" ] && [ -d "$LAST_BACKUP" ]; then
    ok "second install takes one backup of the previous directory"
else
    bad "second install takes one backup of the previous directory"
fi
expect "the backup keeps the version it replaced" "1.0.0" "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["version"])' "$LAST_BACKUP/manifest.json")"

# --- regressions -----------------------------------------------------------
# one backup per install: the update loop used to take a second one on top of
# the one install_archive already takes, leaving four backups for two apps
backups_in_script=$(grep -c 'backup_app "' "$here/extensions-updater.sh" || true)
expect "only install_archive takes backups" "1" "$backups_in_script"

if [ "$failures" -gt 0 ]; then
    printf '\n%s test(s) failed\n' "$failures" >&2
    exit 1
fi
printf '\nall updater tests passed\n'
