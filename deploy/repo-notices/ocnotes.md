# Notice for the `ocnotes` README

> Draft (H6, SPEC §1.2/§8): **prepend** this block to the `ocnotes` repo README
> when the unified release is published, replacing `vX.Y.Z` with the published
> ocapps version (>= the unified 0.1.0 release). After the release, the
> `ocnotes` repo is archived.

---

> ## The backend now lives in `ocapps`
>
> This app's Go backend (`backend/`) is **frozen** at its last commit and **is
> no longer developed**. Its successor is the unified
> **[`ocapps`](https://github.com/gnacho/ocapps) >= vX.Y.Z** service, which
> serves the news, notes and photos backends in a single binary with the same
> API contract (Notes API under `/index.php/apps/notes/api/v1/` and `/v1.4/`,
> plus `/ocs/v2.php/cloud/capabilities` and `/ocs/v2.php/cloud/user`).
>
> - **This repo is still the face of the app**: `extension/`, the README and the
>   Store metadata stay here. Only `backend/` is frozen (do not accept PRs
>   against it).
> - **Users / deployment**: install the `ocapps` *companion server* (static
>   binary + systemd unit + migration script for the `ocnotes` data in
>   [`deploy/`](https://github.com/gnacho/ocapps/tree/main/deploy)) and point the
>   proxy at its single listener (`127.0.0.1:8096`). The `OCNOTES_*` variables
>   are still accepted as legacy for 2 minor versions (up to ocapps 0.3.0) with
>   a warning, so migrate to `OCAPPS_*`. Note that `OCNOTES_GRAPH_URL` (the full
>   `.../graph/v1.0/me` URL) maps to `OCAPPS_OPENCLOUD_URL` by deriving the root
>   automatically.
> - **Store**: the app package declares the `ocapps >= vX.Y.Z` companion server
>   as a requirement (a single ocapps installation serves all three apps, and
>   this app uses the **notes** module). Extension packaging does not change
>   (`vite build`).
