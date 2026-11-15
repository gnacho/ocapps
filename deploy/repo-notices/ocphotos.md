# Notice for the `ocphotos` README

> Draft (H6, SPEC §1.2/§8 + quick-win Q6): **prepend** this block to the
> `ocphotos` repo README when the unified release is published, replacing
> `vX.Y.Z` with the published ocapps version (>= the unified 0.1.0 release).
> After the release, the `ocphotos` repo is archived.

---

> ## The backend now lives in `ocapps`
>
> This app's Go backend (`app/server-go/`) is **frozen** at its last commit and
> **is no longer developed**. Its successor is the unified
> **[`ocapps`](https://github.com/gnacho/ocapps) >= vX.Y.Z** service, which
> serves the news, notes and photos backends in a single binary with the same
> API contract (`/ocphotos-api/api/...`).
>
> - **This repo is still the face of the app**: `ocphotos/` (the extension),
>   `deploy/README.md` and the Store metadata stay here. Only `app/server-go/`
>   is frozen (do not accept PRs against it).
> - **Proxy change when migrating**: the proxy must pass `/ocphotos-api/` to the
>   ocapps upstream (`127.0.0.1:8096`) **WITHOUT strip**, so the trailing slash
>   that stripped the prefix towards the old service has to be removed. The
>   extension already calls `/ocphotos-api/api/...`, so there are no frontend
>   changes.
> - **Environment variables**: `OC_USER` -> `OCAPPS_PHOTOS_USER`,
>   `OC_APP_TOKEN` -> `OCAPPS_PHOTOS_APP_TOKEN`, `MEMORIES_TOKEN` ->
>   `OCAPPS_PHOTOS_TOKEN`, `SCAN_ROOT` -> `OCAPPS_PHOTOS_SCAN_ROOT`,
>   `SCAN_EVERY` -> `OCAPPS_PHOTOS_SCAN_EVERY`, `OC_BASE_URL` ->
>   `OCAPPS_OPENCLOUD_URL`. The legacy names are still accepted for 2 minor
>   versions (up to ocapps 0.3.0) with a warning.
> - **Store**: the app package declares the `ocapps >= vX.Y.Z` companion server
>   as a requirement (a single ocapps installation serves all three apps, and
>   this app uses the **photos** module). Extension packaging does not change
>   (`vite build`).
>
> ## Removal of the legacy PWA (quick-win Q6)
>
> The old PWA is **retired** in the unified release and removed from this repo:
>
> - `app/src`: the legacy PWA (successor: the `ocphotos/` extension).
> - `app/info.md`: documentation of the PWA.
> - `app/Dockerfile`: the all-in-one image (PWA + backend). The supported
>   deployment is the `ocapps` binary or image (which includes `ffmpeg` as the
>   only system dependency) behind your proxy.
>
> The `WEB_DIR` variable goes away with the PWA: if it is defined, ocapps
> ignores it with a warning.
