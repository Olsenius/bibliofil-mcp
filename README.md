# bibliofil-mcp

An [MCP](https://modelcontextprotocol.io) server for Norwegian public libraries running
**Bibliofil**, plus the national search **Biblioteksøk** (bibsok.no).

See your loans and reservations, renew loans, search the catalog, reserve books, and order
books from other libraries. One login covers the whole family folder (*familiemappe*), so you
can act for everyone in it.

## Tools

| Tool | What it does |
|---|---|
| `list_family` | People in the family folder |
| `list_loans` | Current loans: due date, days left, who borrowed it, renewable or not |
| `renew_loans` | Renew loans by id, or all renewable ones |
| `list_reservations` | Local reservations and interlibrary orders |
| `cancel_reservation` | Cancel a reservation or interlibrary order |
| `search_catalog` | Search the local catalog, with availability per branch |
| `reserve` | Reserve a title for a family member |
| `loan_history` | Search the card holder's loan history |
| `search_national` | Search all Norwegian libraries via Biblioteksøk |
| `national_holdings` | Which libraries own a title and how many copies are free |
| `order_interlibrary` | Order a title from another library (only when one can lend it) |
| `list_pickup_locations` | Valid pickup locations, the card's default and the configured one |

## Configuration

Everything is set with environment variables.

| Variable | Required | Description |
|---|---|---|
| `BIBLIOFIL_URL` | yes* | Your library's "Min side", e.g. `https://mitt.<library>.no` (the host serving `/cgi-bin/m2`) |
| `BIBLIOFIL_USER` | yes* | Library card number or e-mail |
| `BIBLIOFIL_PIN` | yes* | PIN / password |
| `MCP_AUTH_TOKEN` | for HTTP | Clients must send `Authorization: Bearer <token>` |
| `BIBLIOFIL_PICKUP` | no | Default pickup branch code for `reserve` |
| `BIBSOK_PICKUP` | no | Default pickup library number for `order_interlibrary` |
| `BIBSOK_URL` | no | Default `https://bibsok.no` |
| `MCP_TRANSPORT` | no | `http` (default) or `stdio` |
| `PORT` | no | Default `8080` |
| `MCP_ALLOW_NO_AUTH` | no | Set to `1` to run HTTP without a token (trusted networks only) |
| `MCP_READ_ONLY` | no | `true` hides the tools that reserve, order, renew and cancel |
| `CONFIG_TOKEN` | no | Token for the settings API below. Defaults to `MCP_AUTH_TOKEN` |
| `DATA_DIR` | no | Where settings saved through the API are kept. Default `/data` |

\* Required either as environment variables or through the settings API. Without them the server still
starts, and the tools answer with an error until the account is set up.

### Settings API and gateway

To use the server from claude.ai or ChatGPT, run it behind [mcp-gateway](https://github.com/Olsenius/mcp-gateway).
The gateway adds OAuth login, and its admin page shows these settings as a form with a **Test** button.
The gateway's repository has a `docker-compose.yml` that sets up both, plus Tailscale Funnel.

The server implements the gateway contract (all but plain `/healthz` need `Authorization: Bearer <CONFIG_TOKEN>`):

| Endpoint | |
|---|---|
| `GET /healthz` | Liveness |
| `GET /healthz?check=1` | Logs in to the library and Biblioteksøk and checks the pickup places |
| `GET /config` | Settings with current values, pickup choices and lock state. The PIN is never returned |
| `POST /config` | `{"values": {"BIBLIOFIL_PIN": "…"}}`. Saved in `DATA_DIR` and applied without restart |

Environment variables always win: a setting given as an environment variable is shown as locked and
cannot be changed through the API.

### Pickup location

If `BIBLIOFIL_PICKUP` / `BIBSOK_PICKUP` are not set, the default from your library card is used.
Biblioteksøk often defaults to the main library, so it is worth setting `BIBSOK_PICKUP` if you
pick up at a branch. Each tool call can also override it with a `pickup` argument.

To find the codes, start the server and call `list_pickup_locations`:

- `kind: "local"` lists branch codes for `BIBLIOFIL_PICKUP`
- `kind: "national"` with `filter: "<town>"` lists library numbers for `BIBSOK_PICKUP`

Each entry shows `card_default` and `configured_default`, so you can see which one is in use.

## Run with Docker

A multi-arch image (amd64, arm64) is built by GitHub Actions on every push to `main`:

```sh
docker pull ghcr.io/olsenius/bibliofil-mcp:latest
```

Or build it yourself with `docker build -t bibliofil-mcp .`, then:

```sh
docker run -d --name bibliofil-mcp -p 8080:8080 \
  -e BIBLIOFIL_URL -e BIBLIOFIL_USER -e BIBLIOFIL_PIN \
  -e BIBLIOFIL_PICKUP -e BIBSOK_PICKUP \
  -e MCP_AUTH_TOKEN -v bibliofil-data:/data \
  ghcr.io/olsenius/bibliofil-mcp:latest
```

The MCP endpoint is `http://<host>:8080/mcp` (stateless streamable HTTP).
It supports MCP `2026-07-28` discovery and per-request metadata, as well as
legacy clients using `initialize`. Health check: `/healthz`.
The image is a static Go binary on distroless (about 20 MB) and runs as non-root.

### Claude Code

```sh
claude mcp add --transport http bibliotek http://localhost:8080/mcp \
  --header "Authorization: Bearer $MCP_AUTH_TOKEN"
```

Or over stdio, without a network port:

```sh
claude mcp add bibliotek -- docker run -i --rm -e MCP_TRANSPORT=stdio \
  -e BIBLIOFIL_URL -e BIBLIOFIL_USER -e BIBLIOFIL_PIN -e BIBLIOFIL_PICKUP -e BIBSOK_PICKUP \
  ghcr.io/olsenius/bibliofil-mcp:latest
```

## Tracing and logging

Set `OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318` to export OpenTelemetry
traces over HTTP/protobuf. Export is disabled without an endpoint. The default
service name is `bibliofil-mcp`; override it with `OTEL_SERVICE_NAME`.
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` can specify a full `/v1/traces` URL instead.
Standard OTLP/HTTP headers, TLS, timeout and resource environment settings are
supported. `OTEL_SDK_DISABLED=true` or `OTEL_TRACES_EXPORTER=none` disables export.
Only the `http/protobuf` export protocol is supported.

Incoming HTTP requests continue W3C trace context from the gateway. Each MCP
method and tool call has its own span, and library HTTP calls are child spans.
MCP failures are marked as errors even when the HTTP response is 200. Spans
include method, route, tool name, hostname, status and duration. They contain no
arguments, results, query strings, headers, PINs or card numbers. Trace headers
and baggage are not sent to the external library sites. Plain `/healthz` calls
are excluded; authenticated checks are traced. MCP spans also work over stdio.
Buffered spans are flushed on graceful shutdown.

The default sampler is `parentbased_always_on`, which follows the gateway's
sampling decision. For a test receiver, use `OTEL_TRACES_SAMPLER=always_on` to
record every request, including clients that send an unsampled parent. For
ratio sampling use `OTEL_TRACES_SAMPLER=parentbased_traceidratio` and
`OTEL_TRACES_SAMPLER_ARG=0.1`.

`LOG_LEVEL` accepts `debug`, `info` (default), `warn`, or `error`. Tracing does
not add request/response body logs at any level. To use the same Jaeger instance
as the gateway, add these variables to the Bibliofil Compose service:

```yaml
environment:
  OTEL_SERVICE_NAME: bibliofil-mcp
  OTEL_EXPORTER_OTLP_ENDPOINT: http://jaeger:4318
  OTEL_EXPORTER_OTLP_PROTOCOL: http/protobuf
  OTEL_TRACES_SAMPLER: always_on
```

In Jaeger, select `bibliofil-mcp`, or open a gateway trace to see both services
in the same call tree.

## Tests

`go test -race ./...` checks discovery, trace propagation through a tool call to
a fake library, error reporting, credential omission, and OTLP export/shutdown.


`./run-live-test.sh` runs read-only checks against your real library in Docker
(set the same environment variables first). It never reserves, orders, renews or cancels.

## How it works

Bibliofil has no public API for patrons. The server logs in to the same web pages a browser uses
(`/cgi-bin/m2`, its `rest_service` JSON calls, and bibsok.no) and reads the HTML. If the library
changes its pages, the parsing may need updates.

- Orders in Biblioteksøk are always for the logged-in card holder.
- Before ordering, the server runs the same availability check as the order page. If no library
  can lend the title right now, it refuses instead of sending an order that goes nowhere.
- Credentials stay in memory and are only sent to the configured library and bibsok.no.

Not affiliated with Bibliotek-Systemer AS, the Norwegian National Library or any library.
