# Aviso para el README de `ocphotos`

> Borrador (H6, SPEC §1.2/§8 + quick-win Q6): **prependear** este bloque
> al README del repo `ocphotos` al publicar la release unificada,
> sustituyendo `vX.Y.Z` por la versión de ocapps publicada (≥ la release
> unificada 0.1.0). Tras la release, el repo `ocphotos` se archiva.

---

> ## ⚠️ El backend vive ahora en `ocapps`
>
> El backend Go de esta app (`app/server-go/`) está **congelado** en su
> último commit y **ya no recibe desarrollo**. Su sucesor es el servicio
> unificado **[`ocapps`](https://github.com/gnacho/ocapps) ≥ vX.Y.Z**, que
> sirve los backends de news, notes y photos en un solo binario con el
> mismo contrato de API (`/ocphotos-api/api/...`).
>
> - **Este repo sigue siendo la cara de la app**: `ocphotos/` (extensión),
>   `deploy/README.md` y metadata de Store se mantienen aquí. Solo
>   `app/server-go/` queda congelado (no aceptar PRs contra él).
> - **Cambio de proxy al migrar**: el proxy debe pasar `/ocphotos-api/`
>   al upstream de ocapps (`127.0.0.1:8096`) **SIN strip** — el trailing
>   slash que strip-peaba el prefijo hacia el servicio antiguo debe
>   quitarse. La extensión ya llama `/ocphotos-api/api/...`, así que no
>   hay cambios en el frontend.
> - **Variables de entorno**: `OC_USER` → `OCAPPS_PHOTOS_USER`,
>   `OC_APP_TOKEN` → `OCAPPS_PHOTOS_APP_TOKEN`, `MEMORIES_TOKEN` →
>   `OCAPPS_PHOTOS_TOKEN`, `SCAN_ROOT` → `OCAPPS_PHOTOS_SCAN_ROOT`,
>   `SCAN_EVERY` → `OCAPPS_PHOTOS_SCAN_EVERY`, `OC_BASE_URL` →
>   `OCAPPS_OPENCLOUD_URL`. Las legacy siguen aceptadas durante 2
>   versiones menores (hasta ocapps 0.3.0) con warning.
> - **Store**: el paquete de la app declara como requisito el companion
>   server `ocapps >= vX.Y.Z` (una sola instalación de ocapps sirve a las
>   tres apps; esta app usa el módulo **photos**). El empaquetado de la
>   extensión no cambia (`vite build`).
>
> ## 🗑️ Retirada de la PWA legacy (quick-win Q6)
>
> La PWA antigua queda **retirada** en la release unificada y se elimina
> de este repo:
>
> - `app/src` — la PWA legacy (sucesora: la extensión `ocphotos/`).
> - `app/info.md` — documentación de la PWA.
> - `app/Dockerfile` — imagen all-in-one (PWA + backend). El despliegue
>   soportado es el binario/imagen de `ocapps` (que incluye `ffmpeg` como
>   única dependencia de sistema) detrás de tu proxy.
>
> La variable `WEB_DIR` desaparece con la PWA: si está definida, ocapps la
> ignora con un warning.
