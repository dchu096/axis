
# Atlas → Axis handoff: Server Shadow

**For:** Axis agent working in this repo (`C:\Users\danie\OneDrive\Documents\axis`)  
**From:** Atlas panel (`Atlas2`) — panel side already shipped  
**Spec:** Atlas `docs/superpowers/specs/2026-10-28-shadow-server-design.md`  
**Panel contract (source of truth for HTTP):** Atlas `docs/reference/axis-shadow-api.md`  
**Panel client:** `app/Repositories/Wings/DaemonShadowRepository.php`  
**Panel remote callback:** `POST /api/remote/shadows/{shadow_uuid}` → `ShadowStatusController`

## What this is

Atlas “Shadow” materializes a **completed backup** into a **files-only** disposable directory on the **node**, so the user can browse **live vs shadow**, diff, and **promote selected paths** onto the live server volume — then discard.

- **Not** a second game container / egg start (unlike DevTools sidecar).
- **Not** full restore (do not truncate live; do not set restoring flag on the live server for the whole tree).
- Shadow bytes **do not** count against `server.disk` quota.
- Disk preflight uses **host/node free space** on the filesystem that holds the shadow, not the server’s billed disk limit.

## Product rules Axis must honor

| Rule | Detail |
|------|--------|
| One shadow per server | Panel enforces one DB row; daemon should also refuse create if a shadow dir/job already exists for that server UUID |
| Location | Outside live volume: prefer `{System.RootDirectory}/shadows/{server_uuid}/` (sibling of `volumes` / `backups`, **not** under `System.Data/{uuid}`) |
| Create | Extract backup into that dir (local adapter or S3 `download_url`) async |
| Preflight | `statfs` / free bytes on shadow filesystem ≥ `required_bytes`; hard refuse if not |
| Mid-extract OOD | Abort, delete partial tree, report `failed` |
| Cancel / Delete | Stop job if running; `rm -rf` shadow dir; leave live untouched |
| List | Directory listing same JSON shape as `GET .../files/list-directory` |
| Promote | Copy listed relative paths from shadow root → live FS root (overwrite/add, recursive dirs). **Do not** start/power the container |
| Callbacks | Notify panel on progress / ready / failed (see below) |

## Panel HTTP contract (implement exactly)

Register under the existing authenticated server group in `router/router.go` (same middleware as backup/files), e.g. `server.Group("/shadow")`:

| Method | Path | Body / query |
|--------|------|----------------|
| `POST` | `/api/servers/:server/shadow/preflight` | `{ "required_bytes": int }` → `{ "ok": bool, "free_bytes"?, "required_bytes"?, "message"? }` |
| `POST` | `/api/servers/:server/shadow` | `{ shadow_uuid, backup_uuid, adapter, download_url, required_bytes }` → `202`, start async |
| `GET` | `/api/servers/:server/shadow` | `{ state, progress_percent?, bytes?, error? }` — `state`: `creating` \| `ready` \| `failed` \| `missing` |
| `POST` | `/api/servers/:server/shadow/cancel` | Abort create + delete dir |
| `DELETE` | `/api/servers/:server/shadow` | Delete dir (discard) |
| `GET` | `/api/servers/:server/shadow/list-directory?directory=/` | Same entries as live list-directory |
| `POST` | `/api/servers/:server/shadow/promote` | `{ "paths": ["a/b", "c"] }` — relative to roots, reject `..` |

### Remote → Panel

Add on Axis `remote.Client`:

`POST {panel}/api/remote/shadows/{shadow_uuid}` with node token (same auth as backup status).

```json
{ "state": "progress", "progress_percent": 40 }
{ "state": "ready", "bytes": 123456789 }
{ "state": "failed", "error": "out of disk" }
```

Panel already has `ShadowStatusController` for this.

## Suggested implementation map (this repo)

Mirror DevTools/backup patterns; keep shadow logic out of the Docker env path.

| Area | Reuse | New |
|------|--------|-----|
| Routes | `router/router.go`, style of `router_server_backup.go` / `router_server_files.go` | `router/router_server_shadow.go` |
| Extract | `postServerRestoreBackup` + `server.RestoreBackup` / `server/backup` local+S3 | Extract **into shadow path**, not live `Filesystem()` root |
| Disk | Host free space (not `Filesystem.HasSpaceAvailable` which is server quota) | `unix.Statfs` on shadow parent, or equivalent |
| Listing | `getServerListDirectory` / filesystem directory listing | List under shadow path with same JSON |
| Promote | File copy primitives in `server/filesystem` | Path-safe copy shadow→live; no power actions |
| State | In-memory per server (like restoring flag) | `creating` / `ready` / mutex + cancel context |
| Remote | `remote/http.go` `SetBackupStatus` / `SendRestorationStatus` | `SetShadowStatus(ctx, shadowUUID, payload)` |

### Recommended layout on disk

```
/var/lib/pterodactyl/shadows/<server_uuid>/   # extracted tree (panel does not care about inner layout beyond "/")
```

Optional config (only if you want it tunable):

```yaml
system:
  shadow_directory: /var/lib/pterodactyl/shadows
```

Default beside `backup_directory` / `data` under `RootDirectory`.

### Create flow (daemon)

1. Preflight already passed (or re-check `required_bytes`).
2. Ensure empty shadow dir for this server (delete leftover first if safe).
3. Async goroutine:
   - Local: `backup.LocateLocal` → extract archive into shadow dir (same archive types as restore: zstd tar / gzip tar).
   - S3: validate `download_url` like restore (`validateBackupDownloadUrl`), stream download, extract into shadow dir.
4. Periodically POST `progress` to panel; on success POST `ready` + `bytes` (tree size); on error POST `failed` and wipe dir.
5. `GET /shadow` reads in-memory state (and `missing` if no dir/job).

**Do not** call `s.SetRestoring(true)` for shadows — that blocks the live server the way full restore does. Panel gates restore/reinstall separately while a shadow row exists.

### Promote safety

- Normalize paths: reject absolute paths, `..`, empty.
- Copy file or directory recursively from `shadowRoot/path` → `liveRoot/path`.
- Overwrite existing live files; create missing parents.
- Live server may be **running** — panel warns the user; Axis still performs the copy (no hard require offline).

### Cancel / delete

- Cancel context of extract goroutine.
- Remove shadow directory entirely.
- Clear in-memory state → subsequent GET returns `missing`.

## Out of scope for Axis

- TTL / idle / extend / one-shadow DB uniqueness (panel + `p:maintenance:sweep-shadows`).
- Vue UI / permissions (`backup.restore`).
- Diff algorithm (panel diffs live list vs shadow list by size+mtime).

## Acceptance checklist

- [ ] Preflight fails when node free space &lt; `required_bytes` with clear `message`
- [ ] Create from local + S3 backup extracts only into shadow dir; live files unchanged
- [ ] Progress + ready/failed callbacks hit panel
- [ ] Cancel mid-create leaves no partial dir
- [ ] Delete removes shadow; GET → `missing`
- [ ] list-directory matches live listing shape
- [ ] Promote copies selected paths onto live; no container start
- [ ] OOD mid-extract → failed + cleanup

## References in Atlas (read-only for you)

- `docs/reference/axis-shadow-api.md`
- `docs/superpowers/specs/2026-10-28-shadow-server-design.md`
- `app/Repositories/Wings/DaemonShadowRepository.php`
- `app/Services/Shadows/ServerShadowService.php`
- `app/Http/Controllers/Api/Remote/Servers/ShadowStatusController.php`

## Closest Axis code to copy from

- Restore entry: `router/router_server_backup.go` → `postServerRestoreBackup`
- Restore extract: `server/backup.go` / `server/backup/*`
- File list: `router/router_server_files.go` → `getServerListDirectory`
- Remote status posts: `remote/http.go` + `remote/servers.go`
- Sidecar feature style (state machine only; **don’t** reuse Docker for shadow): `server/devtools/`
