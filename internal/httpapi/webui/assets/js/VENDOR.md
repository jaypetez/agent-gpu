# Vendored frontend libraries

These are committed, self-hosted, minified third-party libraries the admin console
serves as static assets. They are **never** loaded from a CDN: the console runs
behind operator auth, often on isolated networks, and a strict same-origin policy
(see the `Content-Security-Policy` set in `internal/httpapi/webui.go`) blocks every
external host. Pinning + committing them keeps the build reproducible, offline, and
free of any new runtime dependency.

To bump a version: download the exact pinned file below, replace it here, update the
version + SHA-256 in this file, and verify the new hash. Keep the change in its own
commit so the provenance is auditable.

## `htmx.min.js`

- Library: **htmx** v2.0.4
- Source: `https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js`
- SHA-256: `e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447`

## `alpine.min.js`

- Library: **Alpine.js** v3.14.8
- Source: `https://cdn.jsdelivr.net/npm/alpinejs@3.14.8/dist/cdn.min.js`
- SHA-256: `b600e363d99d95444db54acbfb2deffec9ae792aa99a09229bcda078e5b55643`

Verify locally:

```sh
sha256sum internal/httpapi/webui/assets/js/htmx.min.js internal/httpapi/webui/assets/js/alpine.min.js
```
