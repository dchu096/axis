# Axis — Server Shadow HTTP API (copy of Atlas contract)

Canonical copy also lives in Atlas: `docs/reference/axis-shadow-api.md`.  
Full agent handoff: `docs/atlas-shadow-handoff.md`.

Panel client: Atlas `DaemonShadowRepository`  
Remote callback: `POST /api/remote/shadows/{shadow_uuid}`

Shadow data lives **outside** the live server volume.  
Bytes **do not** count toward `server.disk`. Preflight uses **node free disk**.

## Endpoints (daemon ← panel)

### `POST /api/servers/{server}/shadow/preflight`

```json
{ "required_bytes": 4294967296 }
```

→ `{ "ok": true, "free_bytes": …, "required_bytes": … }` or `{ "ok": false, "message": "…" }`

### `POST /api/servers/{server}/shadow`

```json
{
  "shadow_uuid": "…",
  "backup_uuid": "…",
  "adapter": "wings" | "s3",
  "download_url": "",
  "required_bytes": 4294967296
}
```

Start materialize asynchronously. Abort + delete partial if disk fills.

### `GET /api/servers/{server}/shadow`

```json
{
  "state": "creating" | "ready" | "failed" | "missing",
  "progress_percent": 42,
  "bytes": 0,
  "error": null
}
```

### `POST /api/servers/{server}/shadow/cancel`

### `DELETE /api/servers/{server}/shadow`

### `GET /api/servers/{server}/shadow/list-directory?directory=/`

Same shape as live `files/list-directory`.

### `POST /api/servers/{server}/shadow/promote`

```json
{ "paths": ["world/level.dat", "plugins/OldEconomy.jar"] }
```

## Remote callback (daemon → panel)

`POST /api/remote/shadows/{shadow_uuid}` (node auth)

```json
{ "state": "ready", "bytes": 123456789 }
{ "state": "failed", "error": "…" }
{ "state": "progress", "progress_percent": 40 }
```
