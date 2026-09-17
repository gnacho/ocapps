# Aviso para el README de `ocnews`

> Borrador (H6, SPEC §1.2/§8): **prependear** este bloque al README del
> repo `ocnews` al publicar la release unificada, sustituyendo `vX.Y.Z`
> por la versión de ocapps publicada (≥ la release unificada 0.1.0).
> Tras la release, el repo `ocnews` se archiva.

---

> ## ⚠️ El backend vive ahora en `ocapps`
>
> El backend Go de esta app (`backend/`) está **congelado** en su último
> commit y **ya no recibe desarrollo**. Su sucesor es el servicio unificado
> **[`ocapps`](https://github.com/gnacho/ocapps) ≥ vX.Y.Z**, que sirve los
> backends de news, notes y photos en un solo binario con el mismo
> contrato de API (News API v1.3 bajo `/index.php/apps/news/api/v1-3/`,
> más `/api/me` y `/api/users`).
>
> - **Este repo sigue siendo la cara de la app**: `extension/`, `landing/`,
>   assets y metadata de Store se mantienen aquí. Solo `backend/` queda
>   congelado (no aceptar PRs contra él).
> - **Usuarios / despliegue**: instala el *companion server* `ocapps`
>   (binario estático + unit systemd + script de migración desde los datos
>   de `ocnews` en
>   [`deploy/`](https://github.com/gnacho/ocapps/tree/main/deploy)) y
>   apunta el proxy a su listener único (`127.0.0.1:8096`). Las variables
>   `OCNEWS_*` siguen aceptadas como legacy durante 2 versiones menores
>   (hasta ocapps 0.3.0) con warning; migra a `OCAPPS_*`.
> - **Store**: el paquete de la app declara como requisito el companion
>   server `ocapps >= vX.Y.Z` (una sola instalación de ocapps sirve a las
>   tres apps; esta app usa el módulo **news**). El empaquetado de la
>   extensión no cambia (`vite build`).
