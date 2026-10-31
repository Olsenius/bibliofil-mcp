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
| `BIBLIOFIL_URL` | yes | Your library's "Min side", e.g. `https://mitt.<library>.no` (the host serving `/cgi-bin/m2`) |
| `BIBLIOFIL_USER` | yes | Library card number or e-mail |
| `BIBLIOFIL_PIN` | yes | PIN / password |
| `MCP_AUTH_TOKEN` | for HTTP | Clients must send `Authorization: Bearer <token>` |
| `BIBLIOFIL_PICKUP` | no | Default pickup branch code for `reserve` |
| `BIBSOK_PICKUP` | no | Default pickup library number for `order_interlibrary` |
| `BIBSOK_URL` | no | Default `https://bibsok.no` |
| `MCP_TRANSPORT` | no | `http` (default) or `stdio` |
| `PORT` | no | Default `8080` |
| `MCP_ALLOW_NO_AUTH` | no | Set to `1` to run HTTP without a token (trusted networks only) |

### Pickup location

If `BIBLIOFIL_PICKUP` / `BIBSOK_PICKUP` are not set, the default from your library card is used.
Biblioteksøk often defaults to the main library, so it is worth setting `BIBSOK_PICKUP` if you
pick up at a branch. Each tool call can also override it with a `pickup` argument.

To find the codes, start the server and call `list_pickup_locations`:

- `kind: "local"` lists branch codes for `BIBLIOFIL_PICKUP`
- `kind: "national"` with `filter: "<town>"` lists library numbers for `BIBSOK_PICKUP`

Each entry shows `card_default` and `configured_default`, so you can see which one is in use.

## Run with Docker

```sh
docker build -t bibliofil-mcp .

docker run -d --name bibliofil-mcp -p 8080:8080 \
  -e BIBLIOFIL_URL -e BIBLIOFIL_USER -e BIBLIOFIL_PIN \
  -e BIBLIOFIL_PICKUP -e BIBSOK_PICKUP \
  -e MCP_AUTH_TOKEN \
  bibliofil-mcp
```

The MCP endpoint is `http://<host>:8080/mcp` (streamable HTTP). Health check: `/healthz`.
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
  bibliofil-mcp
```

## Tests

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
