# Despliegue y migración de `ocapps`

Guía para instalar el backend unificado (news + notes + photos) y migrar
desde los tres servicios antiguos (`ocnews`, `ocnotes`, `ocphotos`).
Referencia: [`docs/SPEC.md`](../docs/SPEC.md) §5.4, §8 (H6) y §9.

## Contenido de este directorio

| Fichero | Qué es |
|---|---|
| [`opencloud-apps.service`](opencloud-apps.service) | Unit systemd único (SPEC §9.1) |
| [`env.example`](env.example) | Plantilla de `/etc/ocapps/env` con cada variable y su legacy (SPEC §9.2/§3.2) |
| [`proxy/nginx.conf`](proxy/nginx.conf) · [`proxy/Caddyfile`](proxy/Caddyfile) | Snippets de proxy (SPEC §9.3) |
| [`migrate.sh`](migrate.sh) | Migración de datos 3→1, idempotente, con `--dry-run` y `--force-redeploy` (SPEC §5.4) |
| [`repo-notices/`](repo-notices/) | Borradores de aviso para los README de los repos de app |

## Requisitos

- **Runtime**: Linux con systemd, `sqlite3` (CLI, para las verificaciones de
  la migración), `curl` (smoke checks) y un proxy reverso (nginx o Caddy).
  El binario es estático (`CGO_ENABLED=0`, modernc.org/sqlite): no necesita
  libc ni ninguna otra librería de sistema.
- **ffmpeg es la ÚNICA dependencia de sistema en runtime** (SPEC Q4):
  pósters de vídeo del módulo photos (HLS se eliminó en Q7). Sin ffmpeg el
  servicio arranca igualmente y photos lo loguea como degradado (`ffmpeg no
  encontrado: pósters de vídeo degradados`), pero instálalo:
  `apt install ffmpeg` / `apk add ffmpeg`.
- **Go solo para compilar** (≥ 1.26, ver `go.mod`). No hace falta en el
  host de producción si usas el artefacto de release o la imagen Docker.
- Para la migración: acceso root en el host donde corren los tres
  servicios antiguos.

## Build

### Binario local

```bash
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --tags --always) \
            -X main.commit=$(git rev-parse --short HEAD) \
            -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o ocapps ./cmd/ocapps
```

### Release (goreleaser)

El repo incluye `.goreleaser.yaml` (linux/amd64+arm64, `CGO_ENABLED=0`,
ldflags de versión). `goreleaser release` genera los tarballs + checksums.

### Docker

```bash
docker build -t ocapps .          # etapa runtime: alpine + ffmpeg (SPEC Q4/Q10)
docker run -e OCAPPS_OPENCLOUD_URL=https://cloud.example.com \
           -e OCAPPS_PHOTOS_USER=... -e OCAPPS_PHOTOS_APP_TOKEN=... \
           -v ocapps-data:/var/lib/ocapps -p 127.0.0.1:8096:8096 ocapps
```

La guía de systemd de abajo asume despliegue con binario; con Docker,
adapta el unit a `docker run`/`docker compose` manteniendo las mismas
variables de entorno y el volumen `/var/lib/ocapps`.

## Instalación (binario + systemd)

```bash
# 1. Usuario de servicio
useradd --system --home /var/lib/ocapps --shell /usr/sbin/nologin ocapps

# 2. Binario
install -m 755 ocapps /usr/local/bin/ocapps

# 3. Configuración
install -d -m 750 -o root -g ocapps /etc/ocapps
install -m 640 -o root -g ocapps deploy/env.example /etc/ocapps/env
$EDITOR /etc/ocapps/env        # rellena OCAPPS_OPENCLOUD_URL, PHOTOS_USER, APP_TOKEN...

# 4. Unit
install -m 644 deploy/opencloud-apps.service /etc/systemd/system/
systemctl daemon-reload
# NO lo arranques todavía si vas a migrar datos: migrate.sh lo hace (paso 6).
```

`/var/lib/ocapps` lo crea el propio systemd (`StateDirectory=ocapps`,
0700). El layout de datos es (SPEC §5.1):

```
/var/lib/ocapps/
├── news/     ocnews.db  (+ favicons/, imgcache/, imgsecret, feedsecret)
├── notes/    notes.db   (+ attachments/, imgcache/, imgsecret)
└── photos/   memories.db(+ thumbs/, mediasecret)
```

## Cambio de proxy

> **⚠ Aviso: el strip de `/ocphotos-api/` desaparece.**
>
> Hoy el proxy strip-pea `/ocphotos-api/` (ocphotos ve `/api/...`). En el
> binario unificado `/api/` pertenece a news (`/api/me`, `/api/users`) y
> photos registra sus rutas como `/ocphotos-api/api/...`. El proxy debe
> pasar el prefijo **intacto** al upstream único `127.0.0.1:8096`
> (SPEC §4.3/§9.3). Es **EL cambio de proxy de esta migración** y el único
> paso no revertible por el propio binario: coordínalo con el corte de
> systemd (aplicar el snippet nuevo al mismo tiempo que `migrate.sh`
> habilita `opencloud-apps`, o inmediatamente después).

Aplica [`proxy/nginx.conf`](proxy/nginx.conf) o
[`proxy/Caddyfile`](proxy/Caddyfile) en el vhost que sirve la instancia
OpenCloud. Resumen:

| Ruta | Antes | Ahora |
|---|---|---|
| `/index.php/apps/news/` | → ocnews :8094 (sin strip) | → `127.0.0.1:8096` (sin strip) |
| `/index.php/apps/notes/` | → ocnotes :8100 (sin strip) | → `127.0.0.1:8096` (sin strip) |
| `/ocs/` | → ocnotes :8100 | → `127.0.0.1:8096` |
| `/api/me`, `/api/users` | → ocnews :8094 | → `127.0.0.1:8096` (patrones exactos, **no** catch-all `/api/`) |
| `/ocphotos-api/` | → ocphotos :8097 **con strip** | → `127.0.0.1:8096` **SIN strip** |

En ambos snippets se preservan `Authorization` y `Host`, y
`/ocphotos-api/api/video/` tiene timeouts largos y streaming sin buffer
(Range requests). Tras recargar el proxy, valida con
`nginx -t` / `caddy validate` antes de `reload`.

## Migración desde los 3 servicios antiguos

`deploy/migrate.sh` implementa el procedimiento del SPEC §5.4 paso a paso
(mensajes `==>` en cada paso, `set -euo pipefail`, aborta ante cualquier
verificación fallida). Principios: **copia, nunca mover**; las BDs y datos
origen quedan intactos para rollback; idempotente.

**Ensaya siempre primero con `--dry-run`** (imprime cada acción sin
ejecutar nada):

```bash
sudo deploy/migrate.sh --dry-run
sudo deploy/migrate.sh
```

Pasos que ejecuta:

0. **Pre-vuelo**: comprueba `sqlite3`/`systemctl`/`curl`, root, y **aborta
   si `opencloud-apps` está activo**: sus BDs están vivas y copiar las
   viejas encima las corrompería. Para re-desplegar encima a propósito
   (p. ej. repetir la migración tras un rollback) usa `--force-redeploy`:
   el script para el unit, copia y lo rearranca en el paso 6.
0b. **Para los servicios viejos** (`ocnews ocnotes ocphotos`) y espera a
   que queden inactivos.
1. **Verifica el origen**: `PRAGMA integrity_check` en las tres BDs vivas
   (aborta si alguna no devuelve `ok`) y `PRAGMA wal_checkpoint(TRUNCATE)`
   para una copia auto-contenida aunque quedaran `-wal`/`-shm`.
2. **Copia las BDs** al nuevo layout con `cp -a` (incluye `-wal`/`-shm` si
   existieran). Nunca `mv`.
3. **Copia los datos auxiliares** por módulo (favicons, imgcache,
   imgsecret, feedsecret, attachments, thumbs, mediasecret) y fija la
   propiedad `ocapps:ocapps`.
4. **Verifica el destino**: `integrity_check` en las tres BDs copiadas.
5. **Auditoría de versiones**: registra `PRAGMA user_version` — esperado
   **18** (news), **2** (notes); photos llega sin versionar (0) y ocapps
   aplica el **baseline a 1** en el primer arranque (SPEC §5.3).
6. **systemd**: `disable` de los units viejos (**sin borrarlos**) y
   `enable --now opencloud-apps`.
7. **Smoke checks** por localhost: `/healthz` (los 3 módulos `ok`) y
   `/readyz` (200).

Las rutas de origen son parametrizables con variables al inicio del script
o por entorno (defaults `/var/lib/ocnews`, `/var/lib/ocnotes`,
`/var/lib/ocphotos` — confírmalas contra tu despliegue real, SPEC Anexo
B.2):

```bash
sudo OCNEWS_DIR=/srv/ocnews OCAPPS_DATA_DIR=/var/lib/ocapps deploy/migrate.sh --dry-run
```

Notas:

- La migración 002 de notes (backfill de owner) ya está aplicada en la BD
  viva; `OCAPPS_NOTES_OWNER` solo hace falta si quedaran filas sin owner.
- El primer arranque de ocapps aplica: news/notes no-op, photos baseline.
  Revísalo con `journalctl -u opencloud-apps -n 100`.

## Verificación (checklist SPEC §9.4)

```bash
systemctl is-active opencloud-apps                       # active
systemctl is-enabled ocnews ocnotes ocphotos             # disabled (x3)
journalctl -u opencloud-apps -n 50 --no-pager            # sin "env legacy" inesperados ni módulos failed
curl -s localhost:8096/healthz | jq .                    # los 3 módulos "ok"
curl -s -o /dev/null -w '%{http_code}\n' localhost:8096/readyz   # 200

# Contratos (interno, por localhost, con credenciales válidas):
curl -su user:app-token localhost:8096/index.php/apps/news/api/v1-3/version
curl -su user:app-token localhost:8096/index.php/apps/notes/api/v1/notes
curl -s  -H "Authorization: Bearer $TOK" localhost:8096/ocs/v2.php/cloud/capabilities
curl -s  -H "Authorization: Bearer $TOK" localhost:8096/ocphotos-api/api/stats

# Integridad de BDs:
for f in /var/lib/ocapps/*/*.db; do sqlite3 "$f" 'PRAGMA integrity_check;'; done   # ok x3
sqlite3 /var/lib/ocapps/news/ocnews.db 'PRAGMA user_version;'      # 18
sqlite3 /var/lib/ocapps/notes/notes.db 'PRAGMA user_version;'      # 2
sqlite3 /var/lib/ocapps/photos/memories.db 'PRAGMA user_version;'  # 1 (baseline aplicado)

# Desde el exterior (vía proxy, mismo origen que la web — valida el cambio de proxy):
curl -s  -H "Authorization: Bearer $TOK" https://<host>/ocphotos-api/api/stats
curl -su user:app-token https://<host>/index.php/apps/news/api/v1-3/feeds
```

Verificación funcional final: abrir las tres apps en la web de OpenCloud y
sincronizar un cliente externo (news-android o Iotas).

## Rollback

**Ventana de validez**: el rollback es trivial (sin copia de datos)
**mientras no se hayan escrito datos nuevos que importen en las BDs de
`/var/lib/ocapps`** — los directorios origen `/var/lib/ocnews`,
`/var/lib/ocnotes` y `/var/lib/ocphotos` están intactos (la migración solo
los leyó/copió). Recomendación: decidir rollback vs. keep dentro de las
primeras horas/días tras el corte, antes de que la divergencia de datos
crezca.

### Rollback simple (dentro de la ventana)

```bash
systemctl stop opencloud-apps
systemctl enable --now ocnews ocnotes ocphotos   # sus datos siguen intactos
systemctl disable opencloud-apps                 # opcional; NO borres nada aún
```

Si ya aplicaste el cambio de proxy (§Cambio de proxy), **reviértelo**:
vuelve a enrutar `/ocphotos-api/` → ocphotos :8097 **con strip** (trailing
slash) y los prefijos de news/notes/OCS a sus puertos antiguos; recarga el
proxy. Los datos copiados en `/var/lib/ocapps` pueden conservarse como
respaldo o borrarse cuando el rollback sea definitivo.

### Rollback con datos nuevos escritos tras el corte

Si los usuarios ya generaron datos en las BDs nuevas que no quieres perder,
hay que volcarlos de vuelta (es el procedimiento de migración en sentido
inverso):

```bash
# 1. Parar TODO (nuevo y viejos)
systemctl stop opencloud-apps ocnews ocnotes ocphotos

# 2. Verificar las BDs NUEVAS antes de copiar de vuelta
for f in /var/lib/ocapps/*/*.db; do
  sqlite3 "$f" 'PRAGMA integrity_check;' | grep -qx ok || echo "ABORTA: $f corrupta"
done
sqlite3 /var/lib/ocapps/news/ocnews.db   'PRAGMA wal_checkpoint(TRUNCATE);'
sqlite3 /var/lib/ocapps/notes/notes.db   'PRAGMA wal_checkpoint(TRUNCATE);'
sqlite3 /var/lib/ocapps/photos/memories.db 'PRAGMA wal_checkpoint(TRUNCATE);'

# 3. Copiar de vuelta (cp -a; haz backup de los .db origen antes, por seguridad)
cp -a /var/lib/ocnews/ocnews.db /var/lib/ocnews/ocnews.db.pre-rollback.bak     # ídem notes/photos
cp -a /var/lib/ocapps/news/ocnews.db     /var/lib/ocnews/ocnews.db
cp -a /var/lib/ocapps/notes/notes.db     /var/lib/ocnotes/notes.db
cp -a /var/lib/ocapps/photos/memories.db /var/lib/ocphotos/memories.db
# y los auxiliares que hubieran cambiado (favicons/, attachments/, thumbs/...)

# 4. Verificar de nuevo en los directorios viejos (integrity_check), arrancar
systemctl enable --now ocnews ocnotes ocphotos
systemctl disable opencloud-apps
```

⚠ Limitación conocida: si ocapps aplicó el **baseline de photos**
(`user_version` 0→1), el ocphotos antiguo ignora ese pragma (su esquema es
`CREATE TABLE IF NOT EXISTS`), así que la vuelta es segura. News/notes
llevan el mismo `user_version` que tenían (18/2): sin transformación.

### Después de N días de estabilidad (≥ 7 recomendados, SPEC §5.4)

```bash
# Archivar datos viejos y eliminar los units antiguos (hasta aquí solo
# estaban deshabilitados):
tar -C /var/lib -czf /root/ocapps-migration-archive.tgz ocnews ocnotes ocphotos
rm -f /etc/systemd/system/{ocnews,ocnotes,ocphotos}.service
systemctl daemon-reload
```

## Monitorización

- **`GET /readyz` es la alerta clave**: 200 solo si todos los módulos
  enabled están `ok`; 503 si alguno está `failed`. Un módulo caído NO
  reinicia el proceso (decisión D3, SPEC §4.5): systemd no verá nada
  (`Restart=on-failure` solo actúa si muere el proceso entero).
- `GET /healthz` devuelve 200 siempre que el proceso viva, con el detalle
  por módulo: `{"version":"...","modules":{"news":"ok","notes":"ok","photos":"failed: ..."}}`.
- Logs JSON en journald: `journalctl -u opencloud-apps -f` (busca
  `"módulo failed"` y los warnings `env legacy en uso`).
