# SwaggerHub latest proxy

[![CI](https://github.com/hu553in/swaggerhub-latest-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/hu553in/swaggerhub-latest-proxy/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/hu553in/swaggerhub-latest-proxy)](https://goreportcard.com/report/github.com/hu553in/swaggerhub-latest-proxy)
[![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/hu553in/swaggerhub-latest-proxy)](https://github.com/hu553in/swaggerhub-latest-proxy/blob/main/go.mod)

A tiny HTTP service that, given a short alias, returns the latest published
Swagger spec for an API hosted on SwaggerHub.

```
GET /swagger/users.json   →  the latest users API spec as JSON
GET /swagger/users.yaml   →  same, rendered as YAML
GET /swagger/users.yml    →  alias of .yaml
GET /healthz              →  {"status":"ok"}
```

## Configure

Copy `config.example.yml` to `config.yml` and list your APIs:

```yaml
apis:
  users:
    owner: my-org
    name: users-api
  billing:
    owner: my-org
    name: billing-api
```

Set `SWAGGERHUB_API_KEY` in the environment (or put it directly in the YAML).

The config file path defaults to `./config.yml` and can be overridden via
the `CONFIG_PATH` environment variable.

## Authentication (optional)

The proxy supports four request authentication modes for `/swagger/*`:

- no auth configured;
- API key only via `auth.api_key` or `AUTH_API_KEY`;
- Basic Auth only via `auth.basic_auth` or
  `AUTH_BASIC_AUTH_USERNAME` / `AUTH_BASIC_AUTH_PASSWORD`;
- both enabled at once, in which case either method is accepted.

API key example:

```sh
curl -H "X-API-Key: $AUTH_API_KEY" http://localhost:3000/swagger/users.json
```

Basic Auth example:

```sh
curl -u "$AUTH_BASIC_AUTH_USERNAME:$AUTH_BASIC_AUTH_PASSWORD" \
  http://localhost:3000/swagger/users.json
```

Config example:

```yaml
auth:
  api_key: "dOzZV8zemf/ECttvWuMPAisqkZ1WN7fwKWqryY095iM="
  basic_auth:
    username: "username"
    password: "password"
```

`/healthz` stays public regardless. If only one of
`auth.basic_auth.username` / `auth.basic_auth.password` is set, startup fails
because Basic Auth requires both fields.

## Run

```sh
go run cmd/swaggerhub-latest-proxy/main.go

# or

docker build -t swaggerhub-latest-proxy .
docker run -p 3000:3000 \
  -e SWAGGERHUB_API_KEY=$SWAGGERHUB_API_KEY \
  swaggerhub-latest-proxy

# or

docker run -p 3000:3000 \
  -e SWAGGERHUB_API_KEY=$SWAGGERHUB_API_KEY \
  ghcr.io/hu553in/swaggerhub-latest-proxy
```

## How "latest" is resolved

The proxy pages through every revision of the API and picks the one with
the newest `X-Modified` (or `X-Created`, when modified is missing) timestamp.

Resolved specs are cached in memory for `cache.ttl` (5 min by default).
