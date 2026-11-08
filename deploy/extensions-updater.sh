#!/usr/bin/env bash
# ocapps-ext - install, update and roll back the OpenCloud web extensions that
# ship as release assets (news, notes, ocphotos).
#
# Usage:
#   ocapps-ext list                       show what is installed and its stamp
#   ocapps-ext check                      show latest release per app
#   ocapps-ext install <app> [--tag TAG]  install an app that is not present
#   ocapps-ext update [--dry-run] [--force] [app...]
#   ocapps-ext rollback <app>             restore the previous directory
#
# Stamps: every installed directory carries .ocapps-release with either
#   release:<tag>   installed from a GitHub release
#   local:<date>    built by hand from the repo (the updater never overwrites
#                   it unless --force is given)
#
# Exit codes: 0 ok, 1 usage error, 2 nothing to do, 3 update failed and rolled back.

set -euo pipefail

APPS_DIR=${OCAPPS_EXT_APPS_DIR:-/etc/opencloud/web/assets/apps}
BACKUP_DIR=${OCAPPS_EXT_BACKUP_DIR:-/var/backups/ocapps-ext}
CONF=${OCAPPS_EXT_CONF:-/etc/ocapps/extensions.conf}
APPS_YAML=${OCAPPS_EXT_APPS_YAML:-/etc/opencloud/apps.yaml}
SERVICE=${OCAPPS_EXT_SERVICE:-opencloud}
LOCAL_PORT=${OCAPPS_EXT_PORT:-9200}
GITHUB_TOKEN=${GITHUB_TOKEN:-}
KEEP_BACKUPS=${OCAPPS_EXT_KEEP:-5}

DEFAULT_CONF='news|gnacho/ocnews|^news-[0-9.]+\.zip$
notes|gnacho/ocnotes|^notes-[0-9.]+\.zip$
ocphotos|gnacho/ocphotos|^ocphotos-[0-9.]+\.zip$'

banner() { [ "${QUIET:-0}" = 1 ] || printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

need() {
    command -v "$1" >/dev/null 2>&1 || die "missing dependency: $1"
}

require_root() {
    [ "$(id -u)" = 0 ] || die "run as root"
}

conf_lines() {
    if [ -r "$CONF" ]; then
        grep -vE '^\s*(#|$)' "$CONF"
    else
        printf '%s\n' "$DEFAULT_CONF"
    fi
}

app_meta() {
    local want=$1 id repo re
    while IFS='|' read -r id repo re; do
        [ "$id" = "$want" ] || continue
        printf '%s\n%s\n' "$repo" "$re"
        return 0
    done < <(conf_lines)
    return 1
}

known_apps() { conf_lines | cut -d'|' -f1; }

stamp_of() {
    local f="$APPS_DIR/$1/.ocapps-release"
    [ -r "$f" ] && head -n1 "$f" || echo unknown
}

write_stamp() {
    printf '%s\n' "$2" > "$APPS_DIR/$1/.ocapps-release"
    chown 1000:1000 "$APPS_DIR/$1/.ocapps-release" 2>/dev/null || true
}

# Prints "<tag> <url>" of the newest release carrying an asset that matches the
# app regex. Releases with a different asset (e.g. the collector binaries in
# the same repo) are skipped by matching on the asset name.
latest_release() {
    local repo=$1 re=$2
    local url="https://api.github.com/repos/$repo/releases?per_page=25"
    local args=(-sSfL --max-time 30 -H 'Accept: application/vnd.github+json')
    [ -n "$GITHUB_TOKEN" ] && args+=(-H "Authorization: Bearer $GITHUB_TOKEN")
    curl "${args[@]}" "$url" | RE="$re" python3 -c '
import json, os, re, sys
pattern = re.compile(os.environ["RE"])
releases = json.load(sys.stdin)
releases.sort(key=lambda r: r.get("published_at") or "", reverse=True)
for rel in releases:
    if rel.get("draft"):
        continue
    for asset in rel.get("assets") or []:
        if pattern.match(asset.get("name", "")):
            print(rel["tag_name"], asset["browser_download_url"])
            sys.exit(0)
sys.exit(1)
' || return 1
}

check_asset_path() {
    local unit envf
    unit=$(systemctl show -p FragmentPath --value "$SERVICE" 2>/dev/null || true)
    [ -n "$unit" ] || return 0
    envf=$(grep -ohE '/etc/[^ ]+\.(env|nv)' "$unit" 2>/dev/null | head -1)
    [ -n "$envf" ] && [ -r "$envf" ] || return 0
    if ! grep -qE "^WEB_ASSET_APPS_PATH=$APPS_DIR" "$envf"; then
        banner "warning: $envf does not set WEB_ASSET_APPS_PATH=$APPS_DIR"
        banner "         without it /config.json will not list external apps"
    fi
}

ensure_apps_yaml() {
    local id=$1
    [ -r "$APPS_YAML" ] || : > "$APPS_YAML"
    if ! grep -qE "^${id}:" "$APPS_YAML"; then
        printf '%s:\n  config: {}\n' "$id" >> "$APPS_YAML"
        chown 1000:1000 "$APPS_YAML" 2>/dev/null || true
        banner "  added $id to $APPS_YAML"
    fi
}

verify_app() {
    local id=$1 conf
    conf=$(curl -sSf --max-time 15 "http://127.0.0.1:$LOCAL_PORT/config.json" 2>/dev/null) || return 1
    printf '%s' "$conf" | APPID="$id" python3 -c '
import json, sys, os
app_id = os.environ["APPID"]
data = json.load(sys.stdin)
apps = {a["id"]: a.get("path", "") for a in data.get("external_apps") or []}
if app_id not in apps:
    print(f"  {app_id}: not listed in external_apps", file=sys.stderr)
    sys.exit(1)
print(apps[app_id])
' 
}

restart_and_verify() {
    local ids=("$@") id path url
    if [ "${OCAPPS_EXT_NO_RESTART:-0}" = 1 ]; then
        banner "restart skipped (OCAPPS_EXT_NO_RESTART=1); verify with: ocapps-ext list"
        return 0
    fi
    banner "restarting $SERVICE"
    systemctl restart "$SERVICE"
    for _ in $(seq 1 24); do
        sleep 5
        curl -sf -o /dev/null "http://127.0.0.1:$LOCAL_PORT/status.php" && break
    done
    systemctl is-active --quiet "$SERVICE" || return 1
    for id in "${ids[@]}"; do
        path=$(verify_app "$id") || return 1
        url="http://127.0.0.1:$LOCAL_PORT$path"
        if ! curl -sSf -o /dev/null --max-time 20 "$url"; then
            printf '  %s: %s not reachable\n' "$id" "$path" >&2
            return 1
        fi
        banner "  $id ok ($path)"
    done
}

backup_app() {
    local id=$1 ts
    ts=$(date +%Y%m%d-%H%M%S)
    mkdir -p "$BACKUP_DIR/$ts"
    cp -a "$APPS_DIR/$id" "$BACKUP_DIR/$ts/$id"
    printf '%s\n' "$BACKUP_DIR/$ts/$id"
    ls -1dt "$BACKUP_DIR"/*/ 2>/dev/null | tail -n +$((KEEP_BACKUPS + 1)) | xargs -r rm -rf
}

install_archive() {
    local id=$1 file=$2 tag=$3 tmp
    tmp=$(mktemp -d)
    local root manifest entry
    root=$(python3 - "$file" "$tmp" "$id" <<'PY'
import os, sys, zipfile
src, dst, app_id = sys.argv[1], sys.argv[2], sys.argv[3]
with zipfile.ZipFile(src) as z:
    z.extractall(dst)
# The archive may wrap the build in a directory named after the project
# instead of the app id, so locate the manifest instead of assuming a name.
candidates = [os.path.join(dst, app_id)]
for base, _dirs, files in os.walk(dst):
    if "manifest.json" in files:
        candidates.append(base)
for base in candidates:
    if os.path.isfile(os.path.join(base, "manifest.json")):
        print(base)
        break
else:
    sys.exit("archive does not contain a manifest.json")
PY
) || die "cannot read $id archive"
    manifest="$root/manifest.json"
    entry=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("entrypoint",""))' "$manifest")
    [ -n "$entry" ] || die "manifest without entrypoint in $id"
    [ -f "$root/$entry" ] || die "manifest entrypoint $entry missing in $id"

    if [ -d "$APPS_DIR/$id" ]; then
        local b
        b=$(backup_app "$id")
        banner "  backup: $b"
    fi
    rm -rf "${APPS_DIR:?}/$id"
    cp -a "$root" "$APPS_DIR/$id"
    chown -R 1000:1000 "$APPS_DIR/$id"
    write_stamp "$id" "release:$tag"
    rm -rf "$tmp"
}

cmd_list() {
    local id repo re stamp
    printf '%-10s %-28s %s\n' APP STAMP REPO
    while IFS='|' read -r id repo re; do
        stamp=$(stamp_of "$id")
        printf '%-10s %-28s %s\n' "$id" "$stamp" "$repo"
    done < <(conf_lines)
}

cmd_check() {
    local id repo re out
    printf '%-10s %-22s %-12s %s\n' APP INSTALLED LATEST STATUS
    while IFS='|' read -r id repo re; do
        out=$(latest_release "$repo" "$re" || true)
        local tag=${out%% *} stamp
        stamp=$(stamp_of "$id")
        if [ -z "$out" ]; then
            printf '%-10s %-22s %-12s %s\n' "$id" "$stamp" "-" "no release found"
        elif [ "$stamp" = "release:$tag" ]; then
            printf '%-10s %-22s %-12s %s\n' "$id" "$stamp" "$tag" "up to date"
        elif [[ "$stamp" == local:* ]]; then
            printf '%-10s %-22s %-12s %s\n' "$id" "$stamp" "$tag" "local build, skipped"
        else
            printf '%-10s %-22s %-12s %s\n' "$id" "$stamp" "$tag" "update available"
        fi
    done < <(conf_lines)
}

cmd_install() {
    require_root
    local id=${1:-} tag=${2:-}
    [ -n "$id" ] || die "usage: ocapps-ext install <app> [--tag TAG]"
    local meta repo re
    meta=$(app_meta "$id") || die "unknown app $id (see $CONF)"
    repo=$(printf '%s' "$meta" | sed -n 1p)
    re=$(printf '%s' "$meta" | sed -n 2p)
    local out
    out=$(latest_release "$repo" "$re") || die "no release for $id in $repo"
    local latest_tag=${out%% *} url=${out#* }
    if [ -n "$tag" ]; then
        [ "$tag" = "$latest_tag" ] || die "only the latest release is supported ($latest_tag)"
    fi
    banner "installing $id $latest_tag from $repo"
    local tmp
    tmp=$(mktemp)
    curl -sSfL --max-time 300 -o "$tmp" "$url"
    install_archive "$id" "$tmp" "$latest_tag"
    rm -f "$tmp"
    ensure_apps_yaml "$id"
    check_asset_path
    restart_and_verify "$id"
}

cmd_update() {
    require_root
    local dry=0 force=0 id repo re out tag url stamp ids=() changed=() backups=()
    while [ $# -gt 0 ]; do
        case "$1" in
            --dry-run) dry=1 ;;
            --force) force=1 ;;
            *) ids+=("$1") ;;
        esac
        shift
    done
    if [ ${#ids[@]} -eq 0 ]; then
        mapfile -t ids < <(known_apps)
    fi
    mkdir -p "$BACKUP_DIR"
    for id in "${ids[@]}"; do
        local meta
        meta=$(app_meta "$id") || die "unknown app $id (see $CONF)"
        repo=$(printf '%s' "$meta" | sed -n 1p)
        re=$(printf '%s' "$meta" | sed -n 2p)
        stamp=$(stamp_of "$id")
        out=$(latest_release "$repo" "$re" || true)
        if [ -z "$out" ]; then
            banner "$id: no release found, skipped"
            continue
        fi
        tag=${out%% *}
        url=${out#* }
        if [ "$stamp" = "release:$tag" ]; then
            banner "$id: up to date ($tag)"
            continue
        fi
        if [[ "$stamp" == local:* ]] && [ "$force" != 1 ]; then
            banner "$id: local build ($stamp), kept unless --force"
            continue
        fi
        banner "$id: $stamp -> $tag"
        changed+=("$id")
        if [ "$dry" = 1 ]; then
            continue
        fi
        local tmp
        tmp=$(mktemp)
        curl -sSfL --max-time 300 -o "$tmp" "$url"
        local b
        b=$(backup_app "$id")
        backups+=("$id:$b")
        install_archive "$id" "$tmp" "$tag"
        rm -f "$tmp"
        ensure_apps_yaml "$id"
    done
    if [ ${#changed[@]} -eq 0 ]; then
        banner "nothing to do"
        return 2
    fi
    [ "$dry" = 1 ] && { banner "dry run, nothing changed"; return 0; }
    check_asset_path
    if ! restart_and_verify "${changed[@]}"; then
        printf '%s\n' "verification failed, rolling back" >&2
        local pair
        for pair in "${backups[@]}"; do
            local app=${pair%%:*} dir=${pair#*:}
            rm -rf "${APPS_DIR:?}/$app"
            cp -a "$dir" "$APPS_DIR/$app"
            chown -R 1000:1000 "$APPS_DIR/$app"
            printf '  restored %s\n' "$app" >&2
        done
        systemctl restart "$SERVICE" || true
        exit 3
    fi
}

cmd_rollback() {
    require_root
    local id=${1:-}
    [ -n "$id" ] || die "usage: ocapps-ext rollback <app>"
    local dir
    dir=$(ls -1dt "$BACKUP_DIR"/*/"$id" 2>/dev/null | head -1)
    [ -n "$dir" ] || die "no backup for $id in $BACKUP_DIR"
    banner "restoring $id from $dir"
    rm -rf "${APPS_DIR:?}/$id"
    cp -a "$dir" "$APPS_DIR/$id"
    chown -R 1000:1000 "$APPS_DIR/$id"
    restart_and_verify "$id"
}

main() {
    need curl
    need python3
    need systemctl
    local cmd=${1:-}
    shift || true
    case "$cmd" in
        list) cmd_list ;;
        check) cmd_check ;;
        install) cmd_install "$@" ;;
        update) cmd_update "$@" ;;
        rollback) cmd_rollback "$@" ;;
        *) sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
    esac
}

main "$@"
