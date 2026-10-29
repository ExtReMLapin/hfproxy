# Service levels

Starting objectives for a shared deployment. They are choices, not promises: change the numbers to fit your traffic and your users, then change them in [prometheus-slo-rules.yml](prometheus-slo-rules.yml) too.

| Objective | Indicator | Target over 30 days |
| --- | --- | --- |
| Availability | Download requests (`/models/…`, `/datasets/…`, `/spaces/…`) that do not end in a 5xx | 99.5% |
| Latency | Cache hits whose first byte is sent within 500 ms (`hfproxy_download_ttfb_seconds`, `cache="hit"`) | 95% |

There is no objective for freshness (how soon a change on the Hub becomes visible): hfproxy does not measure it. A ref is confirmed with the Hub at least every `HFPROXY_CACHE_REF_TTL` while the Hub answers.

## What counts

- **Counts against availability:** every 5xx on a download route, including the 502 returned when the Hub is down and the file is not cached. Your users cannot tell why the file did not arrive. If you would rather hold hfproxy to what it controls, subtract `hfproxy_upstream_errors_total` in your own queries; the shipped rules do not.
- **Does not count:** 4xx answers. In particular `429` from the [download limits](operations.md#download-limits) and `401`/`403`/`404` are the caller's doing.
- **Latency** is measured only for successful downloads, from the arrival of the request to the response headers. A cache miss includes the Hub's answer time, which is why the objective covers hits only.

## The indicators as queries

Availability over the last 30 days (needs Prometheus to keep 30 days of data):

```promql
1 - sum(increase(hfproxy_http_requests_total{route=~"/(models|datasets|spaces)/.*", status_code=~"5.."}[30d]))
      / sum(increase(hfproxy_http_requests_total{route=~"/(models|datasets|spaces)/.*"}[30d]))
```

Share of cache hits that started within 500 ms:

```promql
sum(increase(hfproxy_download_ttfb_seconds_bucket{cache="hit", le="0.5"}[30d]))
  / sum(increase(hfproxy_download_ttfb_seconds_count{cache="hit"}[30d]))
```

Error budget left (1 means untouched, 0 means spent, below 0 means missed):

```promql
1 - (1 - <availability above>) / 0.005
```

At 99.5%, a service answering 100,000 downloads in a month may fail 500 of them.

## Burn-rate alerts

Alerting on the monthly figure would warn too late. [prometheus-slo-rules.yml](prometheus-slo-rules.yml) alerts on how fast the budget is being spent, each time over a long window and a short one (the short window makes the alert stop soon after the problem does):

| Alert | Burn rate | Windows | Budget spent if it continues | Severity |
| --- | --- | --- | --- | --- |
| `HfproxyAvailabilityFastBurn` | 14.4 (7.2% errors) | 1 h and 5 min | 2% in 1 hour; gone in 2 days | page |
| `HfproxyAvailabilitySlowBurn` | 6 (3% errors) | 6 h and 30 min | 5% in 6 hours; gone in 5 days | page |
| `HfproxyAvailabilityBudgetDrain` | 3 (1.5% errors) | 1 d and 2 h | 10% in a day | warning |
| `HfproxyCacheHitsSlow` | 6 (30% slow hits) | 6 h and 1 h | | warning |

Each alert also requires about one download a minute, so that a single error on a quiet service does not page anyone. Next to these, [prometheus-alerts.yml](prometheus-alerts.yml) keeps the cause-based alerts (storage down, circuit open, staging full): they usually fire first and tell you why.

## Responding to a burn-rate alert

1. Open the [Grafana dashboard](grafana-observability.json) and the resilience one ([grafana-resilience.json](grafana-resilience.json)): is the error rate steady or a burst, and on which route?
2. Split the 5xx by status (query in [HfproxyServerErrors](runbooks.md#hfproxyservererrors)) and follow the runbook of the cause. A cause outside your control (the Hub is down) still consumes budget: say so in the incident notes.
3. If the budget is spent, stop risky changes (upgrades, setting changes) until the 30-day figure recovers, and review what would have made the failure shorter.
4. After an incident, check that the alert stopped on its own; if it flapped, lengthen the short window rather than silencing it.

## Tuning

- With very low traffic the ratios are noisy: raise the minimum-traffic guard in the rules, or alert only on the long windows.
- Do not set an objective tighter than your dependencies: the Hub, the object store and PostgreSQL bound what hfproxy can deliver for uncached files.
- Revisit the targets after a quarter of real data.
