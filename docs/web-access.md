# Web access

Web access lets you open the LectureFlow desktop app's interface from a browser on another device (for example, an iPhone or tablet) over a private VPN or tailnet such as Tailscale.

It is **off by default** and available in **desktop mode only**.

## Security model

- **Private network / tailnet only — never expose to the public internet.** LectureFlow still listens on `127.0.0.1` only. Put an HTTPS reverse proxy that restricts reachability to your own devices (such as `tailscale serve`) in front of it.
- **Single-key authentication.** On first start with web access enabled, LectureFlow creates `{data_dir}/web-access.key` (256-bit random key, mode `0600`). Signing in at `/web-access/login` sets an `HttpOnly; Secure; SameSite=Strict` HMAC-signed cookie (`lf_web_access`, valid for 30 days). This access key is the only application-level authentication — keep it private.
- **Host allowlist & CSRF guards.** Requests must carry a configured `Host` header (or the web-access loopback address for local testing) and pass same-origin checks.
- **Host-machine actions are blocked.** Mutating requests to host-only endpoints (shutting down the app, granting local paths, moving/importing/cleaning up the data folder, opening the logs folder, app/self updates, restricting data-dir ACLs, native-messaging host registration, and importing component bundles) are refused with HTTP 403 (`web_access.not_available`).

## Setup (example with `tailscale serve`)

1. Add a `[web_access]` section to your `config.toml`:

   ```toml
   [web_access]
   listen = "127.0.0.1:8421"
   # Exact Host header forwarded by your proxy (case-insensitive; include :port unless 443/80).
   hosts = ["mymac.example-tailnet.ts.net:8443"]
   ```

2. Restart the LectureFlow desktop app. Check the log (or your data folder) for the path to `web-access.key`.
3. Expose the loopback listener on your tailnet over HTTPS:

   ```sh
   tailscale serve --https=8443 http://127.0.0.1:8421
   ```

4. On your other device, open `https://mymac.example-tailnet.ts.net:8443/`, enter the key from `web-access.key`, and sign in.

## Rotating the key

Delete `{data_dir}/web-access.key` and restart LectureFlow. A new random key is generated on startup, immediately invalidating all existing browser sessions.

To sign out a single browser, send `POST /web-access/logout` or clear the `lf_web_access` cookie.

## Limitations

- **Desktop mode only.** In standalone web mode, `[web_access]` is ignored with a warning because web mode does not enforce desktop path grants.
- **No local file-path import or folder picker.** A remote browser has no desktop bridge (`window.lectureflow`). Uploading recordings, slides, and syllabi still works via normal browser upload.
- **HTTPS required for browsers.** The session cookie has the `Secure` attribute, so browsers only store and send it over HTTPS (for example, through `tailscale serve`).
