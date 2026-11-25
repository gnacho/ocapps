# Notice for the `ocnews` README

> Draft (H6, SPEC §1.2/§8): **prepend** this block to the `ocnews` repo README
> when the unified release is published, replacing `vX.Y.Z` with the published
> ocapps version (>= the unified 0.1.0 release). After the release, the `ocnews`
> repo is archived.

---

> ## The backend now lives in `ocapps`
>
> This app's Go backend (`backend/`) is **frozen** at its last commit and **is
> no longer developed**. Its successor is the unified
> **[`ocapps`](https://github.com/gnacho/ocapps) >= vX.Y.Z** service, which
> serves the news, notes and photos backends in a single binary with the same
> API contract (News API v1.3 under `/index.php/apps/news/api/v1-3/`, plus
> `/api/me` and `/api/users`).
>
> - **This repo is still the face of the app**: `extension/`, `landing/`, assets
>   and Store metadata stay here. Only `backend/` is frozen (do not accept PRs
>   against it).
> - **Users / deployment**: install the `ocapps` *companion server* (static
>   binary + systemd unit + migration script for the `ocnews` data in
>   [`deploy/`](https://github.com/gnacho/ocapps/tree/main/deploy)) and point the
>   proxy at its single listener (`127.0.0.1:8077`). The `OCNEWS_*` variables
>   are still accepted as legacy for 2 minor versions (up to ocapps 0.3.0) with
>   a warning, so migrate to `OCAPPS_*`.
> - **Store**: the app package declares the `ocapps >= vX.Y.Z` companion server
>   as a requirement (a single ocapps installation serves all three apps, and
>   this app uses the **news** module). Extension packaging does not change
>   (`vite build`).
