# DockPilot — Pre-Merge Test Checklist

**Branch:** `feature/enterprise-dashboard-perf` → merging into `main`
**Scope:** 4 commits / 8 files / +2,852 lines

| Commit | What it adds |
|---|---|
| `b19378a` | Warm background metrics cache, `/api/dashboard.json`, `/healthz`, `/metrics`, structured logs |
| `209303e` | Images, Volumes & Networks management pages |
| `d77529a` | Web terminal, live log streaming, Compose stack management |
| `b6f2581` | CSRF defense + security headers |

**Test target:** `http://192.168.1.177:8090/` (or `http://localhost:8090/`)
**Login:** `admin` / `dockpilot`

> Tick `[x]` as you confirm each item. Anything that fails → note it and don't merge until resolved.

---

## 0. Pre-flight

- [ ] `git checkout feature/enterprise-dashboard-perf && git pull` — up to date with origin
- [ ] Docker/Colima is running (`docker ps` works)
- [ ] `go build -o dockpilot .` — builds clean
- [ ] `go vet ./...` — no warnings
- [ ] `go test ./...` — all pass
- [ ] `go build` race check: `go test -race ./...` — clean
- [ ] Start single instance: `./dockpilot` → log shows `dockpilot listening on :8090`
- [ ] Only **one** dockpilot process running (`pgrep -fl dockpilot`)

---

## 1. Performance cache & observability (`b19378a`)

- [ ] Dashboard `/` loads **fast** (~well under 1s), not the old ~6s
- [ ] First load right after startup shows data (warm cache populated by background loop)
- [ ] Container CPU/mem and host CPU/mem/disk gauges show real values
- [ ] Image count is shown and correct
- [ ] Live metrics refresh on the page **without a full reload** (watch values tick every ~3s)
- [ ] `GET /api/dashboard.json` returns valid JSON (try with `?q=<name>` filter too)
- [ ] `GET /healthz` returns 200 **without** credentials
- [ ] `GET /metrics` returns Prometheus text **without** credentials
- [ ] `/metrics` shows request counters increasing after you click around
- [ ] Server log lines are structured (`method=… path=… status=… dur=… remote=…`)

---

## 2. Images page (`/images`)

- [ ] Page loads; lists local images with Repository / Tag / Image ID / Size / Created
- [ ] KPIs show Total Images, Dangling, Shown
- [ ] Search box filters the list
- [ ] **Pull** a small image (e.g. `hello-world:latest`) → success banner, appears in list
- [ ] **Remove** that image → confirm dialog → success banner → row gone
- [ ] **Force** remove works on a tagged/used image
- [ ] **Prune dangling** runs (confirm dialog) and reports result
- [ ] **Prune all unused** runs (confirm dialog) — ⚠️ test on a throwaway env, frees real space

## 3. Volumes page (`/volumes`)

- [ ] Lists volumes with Name / Driver / Mountpoint / Scope
- [ ] **Create** a volume (e.g. `test-vol`) → success banner, appears
- [ ] Search filters work
- [ ] **Remove** `test-vol` → confirm → success → gone
- [ ] **Prune unused** runs and reports

## 4. Networks page (`/networks`)

- [ ] Lists networks with Name / Driver / Scope / Subnet / Gateway / Containers
- [ ] **Create** a bridge network (e.g. `test-net`) → appears
- [ ] **Remove** `test-net` → confirm → gone
- [ ] Built-in networks (`bridge`, `host`, `none`) have **no Remove button**
- [ ] **Prune unused** runs and reports

---

## 5. Web terminal (`/terminal`)

- [ ] Page lists running containers / lets you pick one
- [ ] Open a shell into a running container → prompt appears
- [ ] Type `ls`, `pwd`, `whoami` → output streams back live
- [ ] Try a container with bash vs only sh — shell selection works
- [ ] Invalid/garbage container id is rejected (no shell injection)
- [ ] Closing the tab / session ends cleanly (`[session closed]`)
- [ ] Exec into a **stopped** container fails gracefully with a message

## 6. Live log streaming (`/logs`)

- [ ] Pick a container → logs appear
- [ ] **Follow mode** streams new lines in real time (generate some: `docker exec <c> sh -c 'while true; do date; sleep 1; done'`)
- [ ] Tail-N selector changes how many lines load initially
- [ ] grep/filter narrows the stream
- [ ] Download button saves the logs
- [ ] Navigating away stops the stream (no leaked process — check `pgrep -f 'docker logs'`)

## 7. Compose stacks (`/stacks`)

- [ ] Page loads; shows Compose version (or a clear "Compose v2 not available" message)
- [ ] Lists existing compose projects with status + running/total service counts
- [ ] Each stack expands to its services (service, container, state, image)
- [ ] **Deploy** form brings up a stack from a compose file path on the host
- [ ] `up` / `pull` / `restart` / `stop` / `down` each work and show success/error banner
- [ ] Stopped projects still appear (recovered from labels)

---

## 8. Security: CSRF + headers (`b6f2581`)

- [ ] Every response carries: `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Content-Security-Policy`
- [ ] `GET /` sets a `dockpilot_csrf=…; SameSite=Strict` cookie
- [ ] **All existing forms still work** in the browser (start/stop/restart/remove, create container, pull image, etc.) — they should, because same-origin
- [ ] Forged cross-site POST is blocked (403):
  ```
  curl -u admin:dockpilot -X POST -H "Origin: http://evil.example.com" \
    -o /dev/null -w "%{http_code}\n" http://localhost:8090/containers/deadbeef/start
  ```
  expect **403**
- [ ] Same-origin POST passes:
  ```
  curl -u admin:dockpilot -X POST -H "Origin: http://localhost:8090" \
    -o /dev/null -w "%{http_code}\n" http://localhost:8090/containers/deadbeef/start
  ```
  expect **303** (not 403)
- [ ] App cannot be embedded in an `<iframe>` from another page (clickjacking blocked)
- [ ] `/healthz` and `/metrics` still reachable without auth (monitoring not broken)

---

## 9. Regression — existing features must still work

- [ ] **Container actions**: start / stop / restart / remove / inspect / logs from the dashboard
- [ ] **Create container** form works
- [ ] **Search/filter** on the dashboard
- [ ] **Runbooks** (`/runbooks`): list, execute, AI-propose, apply
- [ ] **IPAM** (`/ipam`): port mappings + networks view, AI analyze
- [ ] **AI assistant** (Ollama): interpret command, analyze container — confirm Ollama reachable at its configured URL
- [ ] Success/error banners (`?success=` / `?error=`) display correctly
- [ ] Auth: wrong password → 401; right password → access

---

## 10. Merge readiness

- [ ] No leftover debug prints / commented-out code in the diff (`git diff origin/main...HEAD`)
- [ ] No secrets committed (check `dockpilot.auth` is **not** in the diff / is gitignored)
- [ ] All four commit messages are accurate
- [ ] Branch is rebased/up-to-date with `main` (or merge cleanly)
- [ ] Stop dev instances before merging: `pkill -f './dockpilot'`

---

### Quick env reference
| Var | Default | Purpose |
|---|---|---|
| `ADDR` | `:8090` | listen address |
| `AUTH_FILE` | `./dockpilot.auth` | `user:pass` credentials |
| `TRUSTED_ORIGINS` | (unset) | extra allowed origins for CSRF (e.g. prod domain behind proxy) |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon |
| `OLLAMA_BASE_URL` | `http://192.168.1.177:11434/v1` | AI backend |
| `STATS_REFRESH_INTERVAL` | `3s` | warm-cache stats cadence |
