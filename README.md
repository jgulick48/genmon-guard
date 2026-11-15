# genmon-guard

Watches [genmon](https://github.com/jgyates/genmon) and recovers a Generac Evolution
controller from **RPM Sense Loss** (alarm code 1500) start failures. When the generator
fails a start while a start is still demanded, genmon-guard resets the alarm and puts
the controller back in Auto so it cranks again, up to a retry limit. It also sends
its own Slack alerts.

Start demand comes from the inverter's generator dry contact, read from the
[lxp-bridge-go](https://github.com/jgulick48/lxp-bridge-go) Prometheus endpoint
(`lxp_gen_dry_contact` = 1 means a start is wanted).

## How it decides

Every `poll_interval` (default 10s) it reads the start demand, genmon's
`/cmd/status_json` and the newest entry of the Alarm Log (`/cmd/logs_json`).

| Situation | Action |
|---|---|
| New alarm 1500, Switch State `System in Alarm`, demand active, retries left | Alert, wait `retry_delay` (3m), re-check, then `setremote=resetalarm`, `setremote=auto`, and confirm Switch State is `Auto` |
| Same, but retries used up (`max_retries`, default 3, per demand period) | Alert "gave up: needs manual reset"; nothing more until demand drops |
| Demand drops during the delay | No reset; alert that it needs a manual reset |
| Alarm 1500 with no demand, or demand unreadable | Alert only |
| Any other alarm (E-Stop 2800, Low Fuel 2700/2680, ...) | Alert only, never reset |
| Engine running for `success_hold` (2m) after a retry | Alert "recovered", reset the counter |
| genmon or demand source unreadable | No action; one alert after `outage_alert_after` (10m), one when it's back |
| Switch State not `Auto` after a reset, or setremote fails | Alert; stop retrying for this demand period |

Notes:
- `max_retries` counts resets sent. With the default of 3, the generator gets the
  original start plus up to 3 retries; a 4th failure gives up.
- Each alarm-log entry is handled once. State is in memory only. On startup, an alarm
  older than `startup_grace` (5m) counts as already handled, so a restart doesn't
  re-alert or re-reset it. A newer one is handled as usual.
- Only `resetalarm` and `auto` can be sent: the genmon client refuses any other
  remote command (`start`, `stop`, `manual`, ...) before making a request.

### Dry run

`dry_run: true` is the **default**. genmon-guard does everything except send
`setremote`, and its log lines and alerts say "would reset". Run it like this through
at least one real failure before setting `dry_run: false`
(or `GENMON_GUARD_DRY_RUN=false`).

## Configuration

See [`config.example.yaml`](config.example.yaml). Pass it with `-config path`.
Every setting can be overridden from the environment:

| Env | Setting |
|---|---|
| `GENMON_GUARD_GENMON_URL`, `_GENMON_USERNAME`, `_GENMON_USERNAME_FILE`, `_GENMON_PASSWORD_FILE`, `_GENMON_CA_FILE`, `_GENMON_TIMEZONE`, `_GENMON_INSECURE_SKIP_VERIFY` | `genmon.*` |
| `GENMON_GUARD_DEMAND_METRICS_URL`, `_DEMAND_METRIC` | `demand.*` |
| `GENMON_GUARD_SLACK_WEBHOOK_URL_FILE` | `slack.webhook_url_file` |
| `GENMON_GUARD_POLL_INTERVAL`, `_RETRY_DELAY`, `_SUCCESS_HOLD`, `_STARTUP_GRACE`, `_OUTAGE_ALERT_AFTER`, `_RESET_SETTLE` | durations |
| `GENMON_GUARD_MAX_RETRIES`, `_DRY_RUN`, `_LISTEN`, `_LOG_LEVEL`, `_LOG_FORMAT` | the rest |

Secrets never go in the YAML. Put them in files (for example a Kubernetes Secret
mounted at `/etc/genmon-guard/`), or for local runs set `GENMON_GUARD_GENMON_PASSWORD`
and `GENMON_GUARD_SLACK_WEBHOOK_URL`. Surrounding whitespace, including a trailing
newline, is stripped from secret files.

### genmon versions

The client works with both genmon API styles and detects which one it is talking to
the first time it sends `setremote`:

- **Release 2.0.03 and earlier:** `/cmd/<command>` is GET-only and there is no
  `csrf_token` command. `setremote` is sent as
  `GET /cmd/setremote?setremote=resetalarm`.
- **Current master:** `setremote` must be a POST with an `X-CSRFToken` header from
  `GET /cmd/csrf_token`.

A read-only check against a real genmon (it never sends `setremote`):

```sh
GENMON_LIVE_URL=https://genmon.example.lan GENMON_LIVE_USER=admin GENMON_LIVE_PASSWORD=... \
  go test -tags live -run TestLive -v ./internal/genmon/
```

The genmon login needs **write access** (the admin user); a read-only user gets
`Read Only Mode` on `setremote`. Failed logins back off (1m doubling to 15m) because
genmon locks the UI after repeated failures.

## Endpoints

On `listen` (default `:8080`):

- `/healthz`: 200 while the poll loop is running.
- `/metrics`: Prometheus text format.
  - `genmon_guard_retries_total{result="sent|recovered|gave_up"}`
  - `genmon_guard_demand`
  - `genmon_guard_demand_up`
  - `genmon_guard_genmon_up`
  - `genmon_guard_last_alarm_code`
  - `genmon_guard_dry_run`
  - `genmon_guard_alert_errors_total`

## Build

```sh
make test          # go test ./...
make build         # bin/genmon-guard
make docker.push   # multi-arch (amd64+arm64) image via the shared buildx builder
                   # -> docker.io/jgulick48/genmon-guard:<YYYY.MM.DD-HHMM>
```

The image is a static binary on `gcr.io/distroless/static-debian12:nonroot` and runs as
uid 65532. Its default command is `-config=/etc/genmon-guard/config.yaml`.

## Layout

- `internal/guard`: the decision state machine (`machine.go`, pure and table-tested)
  and the poll loop that does the I/O (`runner.go`).
- `internal/genmon`: the genmon web API client (login, cookie session, CSRF,
  re-login, log parsing). `genmontest` is an `httptest` fake of genmon.
- `internal/demand`: the Prometheus text scraper for the dry-contact gauge.
- `internal/alert`: the `Alerter` interface, plus Slack and log implementations.
- `internal/config`: YAML, env overrides and secrets.
- `internal/metrics`: a minimal Prometheus registry.
