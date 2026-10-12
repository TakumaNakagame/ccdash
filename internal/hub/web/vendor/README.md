# Vendored browser libraries

The portal used to load these from cdn.jsdelivr.net. They are embedded in
the ccdash binary instead (ADR 0005), so neither the hub nor the local
portal (`ccdash web`) makes a request to a CDN. Each directory carries the
version in its name — the hub serves `/vendor/` as immutable — and the
package's own license file.

Every file is copied byte for byte from the npm registry tarball below
(integrity checked against the registry's `dist.integrity`).

| Directory | Package | Files taken | License |
|---|---|---|---|
| `xterm@5.5.0/` | [@xterm/xterm 5.5.0](https://registry.npmjs.org/@xterm/xterm/-/xterm-5.5.0.tgz) | `lib/xterm.js`, `css/xterm.css`, `LICENSE` | MIT |
| `addon-fit@0.10.0/` | [@xterm/addon-fit 0.10.0](https://registry.npmjs.org/@xterm/addon-fit/-/addon-fit-0.10.0.tgz) | `lib/addon-fit.js`, `LICENSE` | MIT |
| `addon-web-links@0.11.0/` | [@xterm/addon-web-links 0.11.0](https://registry.npmjs.org/@xterm/addon-web-links/-/addon-web-links-0.11.0.tgz) | `lib/addon-web-links.js`, `LICENSE` | MIT |
| `marked@12.0.2/` | [marked 12.0.2](https://registry.npmjs.org/marked/-/marked-12.0.2.tgz) | `marked.min.js`, `LICENSE.md` | MIT |
| `dompurify@3.4.16/` | [dompurify 3.4.16](https://registry.npmjs.org/dompurify/-/dompurify-3.4.16.tgz) | `dist/purify.min.js`, `LICENSE` | Apache-2.0 (dual MPL-2.0 OR Apache-2.0; used under Apache-2.0) |

Tarball integrity (npm `dist.integrity`):

```
@xterm/xterm@5.5.0              sha512-hqJHYaQb5OptNunnyAnkHyM8aCjZ1MEIDTQu1iIbbTD/xops91NB5yq1ZK/dC2JDbVWtF23zUtl9JE2NqwT87A==
@xterm/addon-fit@0.10.0         sha512-UFYkDm4HUahf2lnEyHvio51TNGiLK66mqP2JoATy7hRZeXaGMRDr00JiSF7m63vR5WKATF605yEggJKsw0JpMQ==
@xterm/addon-web-links@0.11.0   sha512-nIHQ38pQI+a5kXnRaTgwqSHnX7KE6+4SVoceompgHL26unAxdfP6IPqUTSYPQgSwM56hsElfoNrrW5V7BUED/Q==
marked@12.0.2                   sha512-qXUm7e/YKFoqFPYPa3Ukg9xlI5cyAtGmyEIzMfW//m6kXwCy2Ps9DYf5ioijFKQ8qyuscrHoY04iJGctu2Kg0Q==
dompurify@3.4.16                sha512-sqo+pNp3qRhCIpbgRi1y8Tgk27Bo2Ry7w0dC1NBeNTdZChWjz9Xb/KOoZbRP/R6pQZ80Qw8YhXw13hWWBbMRnQ==
```

xterm.js, its addons and marked are the versions the CDN tags pinned.
DOMPurify moved from 3.1.6 to 3.4.16 when it was vendored (the old one has
published sanitizer bypasses); the API used (`DOMPurify.sanitize`) is the
same.

To update one: download the new tarball from the registry, check its
integrity, copy the same files into a new `<name>@<version>/` directory,
delete the old directory, and update the references in `../index.html` /
`../app.js` and this table. `TestVendoredAssets` fails if a reference
points at a file that isn't embedded.
