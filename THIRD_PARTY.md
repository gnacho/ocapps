# Third-party software

`ocapps` is distributed under the AGPL-3.0-only license (see [`LICENSE`](LICENSE)).

This inventory lists the modules the build and the tests actually compile, and
the license each upstream declares. It is generated, do not edit it by hand:

```sh
go run tools/thirdparty/main.go > THIRD_PARTY.md
```

CI regenerates it and fails if the committed file is out of date, so a new or
upgraded dependency cannot land without being listed here.

## Vendored code

`third_party/goexif` is a modified copy of
[`rwcarlsen/goexif`](https://github.com/rwcarlsen/goexif), used through a
`replace` directive. Upstream is BSD-2-Clause, and its license text is kept
next to the code in [`third_party/goexif/LICENSE`](third_party/goexif/LICENSE).

No other dependency is vendored: everything else is fetched from the Go module
proxy at build time.

## Modules

| Module | Version | License |
|---|---|---|
| `github.com/PuerkitoBio/goquery` | v1.12.0 | BSD-3-Clause |
| `github.com/andybalholm/cascadia` | v1.3.3 | BSD-2-Clause |
| `github.com/araddon/dateparse` | v0.0.0-20210429162001-6b43995a97de | MIT |
| `github.com/aymerick/douceur` | v0.2.0 | MIT |
| `github.com/dustin/go-humanize` | v1.0.1 | MIT |
| `github.com/gen2brain/h265` | v0.2.2 | MIT |
| `github.com/go-shiori/dom` | v0.0.0-20230515143342-73569d674e1c | MIT |
| `github.com/go-shiori/go-readability` | v0.0.0-20251205110129-5db1dc9836f0 | MIT |
| `github.com/gogs/chardet` | v0.0.0-20211120154057-b7413eaefb8f | MIT |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause |
| `github.com/gorilla/css` | v1.0.1 | BSD-3-Clause |
| `github.com/microcosm-cc/bluemonday` | v1.0.27 | BSD-3-Clause |
| `github.com/mmcdole/gofeed` | v1.4.1 | MIT |
| `github.com/mmcdole/goxpp/v2` | v2.0.0 | MIT |
| `github.com/remyoudompheng/bigfft` | v0.0.0-20230129092748-24d4a6f8daec | BSD-3-Clause |
| `github.com/rwcarlsen/goexif` | v0.0.0-20190401152319-9e8de2bddbd4 | BSD-2-Clause |
| `golang.org/x/crypto` | v0.58.0 | BSD-3-Clause |
| `golang.org/x/image` | v0.46.0 | BSD-3-Clause |
| `golang.org/x/net` | v0.62.0 | BSD-3-Clause |
| `golang.org/x/sync` | v0.24.0 | BSD-3-Clause |
| `golang.org/x/sys` | v0.49.0 | BSD-3-Clause |
| `golang.org/x/text` | v0.43.0 | BSD-3-Clause |
| `modernc.org/libc` | v1.75.7 | BSD-3-Clause |
| `modernc.org/mathutil` | v1.7.1 | BSD-3-Clause |
| `modernc.org/memory` | v1.12.1 | BSD-3-Clause |
| `modernc.org/sqlite` | v1.59.0 | BSD-3-Clause |

The license column is detected from the license file the module ships in the
module cache. `unrecognized` means no license file was found, or its text did not
match a known signature: check upstream before relying on that module.
