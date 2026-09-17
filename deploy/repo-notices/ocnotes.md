# Aviso para el README de `ocnotes`

> Borrador (H6, SPEC §1.2/§8): **prependear** este bloque al README del
> repo `ocnotes` al publicar la release unificada, sustituyendo `vX.Y.Z`
> por la versión de ocapps publicada (≥ la release unificada 0.1.0).
> Tras la release, el repo `ocnotes` se archiva.

---

> ## ⚠️ El backend vive ahora en `ocapps`
>
> El backend Go de esta app (`backend/`) está **congelado** en su último
> commit y **ya no recibe desarrollo**. Su sucesor es el servicio unificado
> **[`ocapps`](https://github.com/gnacho/ocapps) ≥ vX.Y.Z**, que sirve los
> backends de news, notes y photos en un solo binario con el mismo
> contrato de API (Notes API bajo `/index.php/apps/notes/api/v1/` y
> `/v1.4/`, más `/ocs/v2.php/cloud/capabilities` y `/ocs/v2.php/cloud/user`).
>
> - **Este repo sigue siendo la cara de la app**: `extension/`, README y
>   metadata de Store se mantienen aquí. Solo `backend/` queda congelado
>   (no aceptar PRs contra él).
> - **Usuarios / despliegue**: instala el *companion server* `ocapps`
>   (binario estático + unit systemd + script de migración desde los datos
>   de `ocnotes` en
>   [`deploy/`](https://github.com/gnacho/ocapps/tree/main/deploy)) y
>   apunta el proxy a su listener único (`127.0.0.1:8096`). Las variables
>   `OCNOTES_*` siguen aceptadas como legacy durante 2 versiones menores
>   (hasta ocapps 0.3.0) con warning; migra a `OCAPPS_*`. Ojo:
>   `OCNOTES_GRAPH_URL` (URL completa `.../graph/v1.0/me`) se mapea a
>   `OCAPPS_OPENCLOUD_URL` derivando la raíz automáticamente.
> - **Store**: el paquete de la app declara como requisito el companion
>   server `ocapps >= vX.Y.Z` (una sola instalación de ocapps sirve a las
>   tres apps; esta app usa el módulo **notes**). El empaquetado de la
>   extensión no cambia (`vite build`).
