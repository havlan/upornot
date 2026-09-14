# upornot

A small public uptime checker written with Go's standard HTTP and HTML tooling. Add a public web address, see its current response and last check time, then let the service check it again every five minutes.

## Run locally

With no configuration, data stays in memory:

```sh
go run .
```

Open <http://localhost:8080>. On this machine, Go is available in Ubuntu WSL:

```sh
wsl -d Ubuntu -- bash -lc 'cd /mnt/c/path/to/upornot && go run .'
```

To persist data in PostgreSQL, start the supplied database and run the app with `DATABASE_URL`:

```sh
docker compose up -d postgres
DATABASE_URL='postgres://upornot:upornot@localhost:5432/upornot?sslmode=disable' go run .
```

The app creates its `sites` table automatically. `CHECK_INTERVAL` defaults to `5m`; `CHECK_TIMEOUT` defaults to `10s`.

New-site submissions are limited to five per client IP each minute. The limiter is intentionally in memory because production runs one replica; use a shared gateway or limiter before scaling beyond one replica.

## API

```sh
curl http://localhost:8080/api/sites
curl -X POST http://localhost:8080/api/sites \
  -H 'content-type: application/json' \
  -d '{"url":"github.com"}'
```

`POST /api/sites` checks the address immediately. `GET /api/sites` returns the saved watchlist. `GET /healthz` is ready for a container health probe.

## Azure Container Apps

The `Dockerfile` is ready for a Container App. Provision a PostgreSQL Flexible Server, create its database, and keep the connection string out of source control:

```sh
az extension add --name containerapp --upgrade
az group create --name upornot-rg --location westeurope
az containerapp up --name upornot --resource-group upornot-rg --location westeurope \
  --source . --ingress external --target-port 8080

az containerapp secret set --name upornot --resource-group upornot-rg \
  --secrets database-url="$DATABASE_URL"
az containerapp update --name upornot --resource-group upornot-rg \
  --min-replicas 1 --max-replicas 1 \
  --set-env-vars DATABASE_URL=secretref:database-url CHECK_INTERVAL=5m
```

Use a PostgreSQL URL with `sslmode=require` for Azure Database for PostgreSQL. Keeping one replica running is intentional: scheduled checks do not run while a scale-to-zero app is asleep.

## Limits

Only public HTTP(S) targets on ports 80 and 443 are allowed. Redirects are capped at five hops, and every connection resolves and rejects private, loopback, link-local, carrier-grade NAT, and benchmark addresses before dialing.
