# Test stack: Keycloak, Prometheus and Grafana

`docker-compose.test.yml` starts hfproxy together with a Keycloak to sign in with and a Prometheus and Grafana to watch it. It is meant for trying things out on a workstation: the credentials are fixed and public, and every port is bound to `127.0.0.1`. Do not expose it. For everyday development use `docker-compose.yml`.

```bash
docker compose -f docker-compose.test.yml up --build -d
docker compose -f docker-compose.test.yml down -v   # stop and forget everything
```

| Service | Address | Accounts |
| --- | --- | --- |
| hfproxy | http://localhost:8080 | first administrator: setup code `test-setup-code-0123456789`; Keycloak users sign in with **Sign in with OIDC** |
| Keycloak | http://localhost:8180 | console `admin` / `admin`; realm `hfproxy` with `alice`, `bob` and `erin` (password `<name>-password`; `erin`'s email is not verified) |
| Grafana | http://localhost:3000 | `admin` / `admin`, or **Sign in with Keycloak** (`alice` is an administrator, others are viewers); the *hfproxy observability* dashboard is already there |
| Prometheus | http://localhost:9091 | scrapes hfproxy every 5 seconds and loads [prometheus-alerts.yml](prometheus-alerts.yml) |

## Things to try

1. Open hfproxy, create the first administrator with the setup code, then sign out and sign in as `alice` through Keycloak: a reader account is created. `erin` is refused, because provisioning needs a verified email.
2. Point a client at the proxy and download something, twice, to see a miss then a hit:
   ```bash
   HF_ENDPOINT=http://localhost:8080 huggingface-cli download gpt2 config.json
   ```
   Set `HFPROXY_HF_TOKEN` in your shell before `docker compose up` for private or gated repositories.
3. Watch the dashboard in Grafana. In hfproxy's *Settings* you can change the log level, the download limits and the cache size and see the effect.

## How it fits together

The browser and hfproxy have to reach Keycloak at the same address, because the issuer URL is inside every token. hfproxy therefore runs in Keycloak's network namespace (`network_mode: service:keycloak`): for both of them `http://localhost:8180` is Keycloak. The consequences are that hfproxy's published port is declared on the `keycloak` service, and that Prometheus scrapes hfproxy's metrics port as `keycloak:9090`. Grafana, which is not in that namespace, reaches Keycloak as `keycloak:8180` for its server-side calls and sends your browser to `localhost:8180`.
