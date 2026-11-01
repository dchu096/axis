# Axis

**Axis** is the Wings-compatible node daemon for **Atlas Panel** — game server containers, files, backups, SFTP, and a locked-down **DevTools** sidecar.

Drop Axis onto a node the same way you would Wings. Atlas speaks the same node API, so you do not reinvent the control plane.

---

## Features

| Area | What you get |
| --- | --- |
| **API** | Power, console, files, backups, transfers (TUS + archive fallback), installs |
| **Analytics** | `GET .../files/disk-usage` TreeSize tree for Atlas Analytics |
| **Backups** | Local or S3/R2 **multipart `.tar.zst`** archives |
| **SFTP** | Built-in SFTP with the same credentials users see in Atlas |
| **DevTools** | Per-server sidecar for `git` / `curl` / file work — not a jailbreak shell on the game container |
| **Compatibility** | Wings-compatible config path and node protocol |

---

## Relationship to Wings

Axis is a fork of [Pterodactyl Wings](https://github.com/pterodactyl/wings), oriented at Atlas Panel.

- Upstream Wings install and ops guides still apply for most node setup.
- Axis-specific behavior (branding, DevTools, Atlas-facing extras) lives in this repository.
- Security hardening is tracked against Wings **v1.13.3** — see [CHANGELOG](CHANGELOG.md).

---

## Quick start

### Build

```bash
make build
# → build/axis_linux_amd64
# → build/axis_linux_arm64
```

Or:

```bash
go build -o axis .
```

### Configure

Default config path (Wings-compatible): `/etc/pterodactyl/config.yml`

Minimal DevTools block:

```yaml
system:
  devtools:
    enabled: true
    image: ghcr.io/dchu096/atlas-devtools:latest
    memory: 512
    cpu: 100
    idle_timeout: 900
    ssh_bind: "0.0.0.0"
    # Non-empty = bind only from this pool; empty = random high port
    ports: [40000, 40001, 40002]
```

Backup compression (multipart S3 + local). Always **zstd** (`.tar.zst`).
Legacy `.tar.gz` restores still work. Remove any `format:` / `compression_level:`
lines from node `config.yml` — they are ignored.

```yaml
system:
  backups:
    zstd_level: 3
```

### DevTools image

```bash
docker build -t ghcr.io/dchu096/atlas-devtools:latest -f devtools/Dockerfile devtools
# or
docker pull ghcr.io/dchu096/atlas-devtools:latest
```

### Docker (optional)

Published image: `ghcr.io/atlas-panel/axis`

See [`docker-compose.example.yml`](docker-compose.example.yml) for a host mount layout compatible with existing Wings nodes.

---

## Security

Axis includes upstream Wings fixes through **v1.13.3**, including:

- SFTP streaming writes enforce per-server disk quotas (`GHSA-hfvm-879c-xgmm` / `GHSA-8j54-xcwx-597p`)
- Integer-overflow-safe quota math on upload / decompress (`GHSA-5mgr-9frx-2rh7`)
- SFTP Setstat DoS guard (`GHSA-ghrq-5wpp-hxx5`)
- `chmod` does not follow symlinks (`GHSA-rhq6-9rgh-v45c`)
- JWT purpose scoping, backup ID canonicalization, S3 restore SSRF blocks, and restricted `{{config.*}}` templating

After upgrading from an older Wings/Axis build that may have leaked a daemon token via egg templating, **rotate the node token** in the panel and redeploy `config.yml`.

---

## Documentation

- [Wings install guide](https://pterodactyl.io/wings/1.0/installing.html) (ops baseline)
- [Changelog](CHANGELOG.md)

---

## License

MIT — see [LICENSE](LICENSE). Based on Pterodactyl Wings.
