# easyagent-ext-ui-host

Serves extension panels: **installs them from a GitHub release, then serves them.**

Deployment and discovery are one service because they are one job. A panel that is installed but not
discovered is invisible in the console; one that is discovered but not installed is a card that opens
nothing. Two services would mean two places to look when a panel does not appear.

## Where it sits

```
                    ingress
                       │
        ┌──────────────┴───────────────┐
        │                              │
  /ext/<id>/                    /ext/<id>/api/
        │                              │
        ▼                              ▼
  ext-ui-host                    <id>-ext
  (panel + discovery)            (the extension's own API)
```

**One rule, split by path.** That split is what makes this work at all: the panel is *same-origin* with
the console, so it reads the bearer from `localStorage` exactly as the console does — and the
extension's API keeps its own authentication, unchanged.

The extension's Go binary no longer embeds a UI. It is a backend, and this service is the front end for
every extension at once.

## Install a panel

```sh
curl -X POST http://host/admin/ui.v1.UiHostService/Install \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \
  -d '{"extId":"gitdev","repo":"easydev-org/easyagent-gitdev-ext-ui","version":"latest"}'
```

```json
{"extId":"gitdev","version":"v0.1.5","files":315,"bytes":"1944241",
 "sha256":"ea8595ef...","info":{"title":"Git","descriptions":{...}}}
```

**`version` may be a tag or `latest`**, and `latest` is resolved **once**, at install time. A floating
tag re-resolved per request would make the bytes a browser gets depend on when they asked.

**The digest in the response is what the next install can pin** via `sha256`, so verifying the supply
chain is a value in hand rather than one nobody wrote down.

## Install does not activate

```sh
curl -X POST http://host/admin/ui.v1.UiHostService/Activate \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \
  -d '{"extId":"gitdev","version":"v0.1.5"}'
```

The separation is the whole safety story: installing writes a new directory and touches nothing a
reader can see, so a broken package can be installed, looked at, and thrown away. `Activate` is a single
`rename` of a symlink — atomic, and reversible because the old version is still on disk.

## Remove

```sh
curl -X POST http://host/admin/ui.v1.UiHostService/Remove \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \
  -d '{"extId":"gitdev","version":"v0.1.4"}'
```

**Refuses the serving version.** Deleting what serves leaves a dangling symlink and every request a
404, which from the console is indistinguishable from a broken panel. Activate something else first.

## The panel declares itself

The bundle carries a `ui.json` at its root:

```json
{
  "title": "Git",
  "descriptions": {"en": "...", "zh": "..."},
  "icon": "/icon.svg"
}
```

This is what `/ext/<id>/api/v1/ui` — the console's discovery probe — answers from. It lives in the
**panel**, not in the extension's binary: a wording change is a panel release, not a backend release.

The response shape is the one the console already reads:

```json
{"base":"/ext/gitdev","id":"gitdev","title":"Git","icon":"/icon.svg","descriptions":{...}}
```

## On disk

```
/data/
  gitdev/
    v0.1.4/
      dist/...            the unpacked bundle
      install.json        where it came from, its digest, when
    v0.1.5/
    current -> v0.1.5     the symlink that decides what serves
```

Every version in its own directory is what makes an install safe to attempt and a rollback free.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `UI_DATA_DIR` | `/data` | Where panels live. **Refuses to start if it does not exist** — a missing volume would otherwise look like every extension losing its UI. |
| `PORT` | `8080` | |
| `UI_BASE` | `/ext` | The mount this service owns. The ingress must send this here and `<base>/<id>/api/` to the extension. |
| `ADMIN_TOKEN` | *(empty)* | Bearer required on `/admin`. Empty means **open**, and that is logged at startup: installing a panel writes files the console then executes. |
| `GITHUB_TOKEN` | *(empty)* | Raises the rate limit and is required for a private panel repository. |

## Development

```sh
go test ./... -race
go build -o /tmp/ext-ui-host ./cmd/ext-ui-host
mkdir -p /tmp/uidata
UI_DATA_DIR=/tmp/uidata PORT=18099 go run ./cmd/ext-ui-host
```

The proto is the contract: `proto/ui/v1/host.proto`, generated into `gen/` by `buf generate`. CI fails
when the committed output is stale.
