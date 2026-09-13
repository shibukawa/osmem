---
title: "Management API"
description: "The /_osmem endpoints and the osmem-server process contract, for writing your own helper."
---

The language packages are thin: each spawns `osmem-server`, reads one line, and calls a handful of endpoints under `/_osmem`. This page is the contract they rely on, for anyone writing a helper for another language or test framework.

## Endpoints

| request | effect | response |
|---|---|---|
| `GET /_osmem` | server info | `{"version","frozen","clones"}` |
| `GET /_osmem/base` | base state | `{"frozen","clones","indices"}` |
| `POST /_osmem/base/freeze` | reject writes to the base | `{"acknowledged":true,"frozen":true}` |
| `POST /_osmem/base/unfreeze` | allow writes again | `{"acknowledged":true,"frozen":false}` |
| `POST /_osmem/clones` | clone the base (freezing it), serve the clone on a new loopback port | `201 {"id","url"}` |
| `GET /_osmem/clones` | list clones | `{"clones":[{"id","url","created"}]}` |
| `GET /_osmem/clones/{id}` | one clone | `{"id","url","created"}` or 404 |
| `DELETE /_osmem/clones/{id}` | close the clone and its port | `{"acknowledged":true}` or 404 |
| `DELETE /_osmem/clones` | close all clones | `{"acknowledged":true,"closed":n}` |

A clone is a complete cluster with its own port, used through the plain OpenSearch API. Closing a clone never affects the base, while stopping the server closes every clone it created.

## What a frozen base still accepts

`GET`, `HEAD` and `OPTIONS` always pass. Among `POST`, `PUT` and `DELETE`, requests whose path contains `_search`, `_count`, `_msearch`, `_mget`, `_analyze`, `_explain`, `_validate`, `_field_caps`, `_refresh`, `_flush`, `_forcemerge`, `_cache`, `_stats`, `_cat`, `_cluster` or `_nodes` pass, which includes scroll and point-in-time handling. Everything else returns:

```json
{"error": {"type": "osmem_base_frozen", "reason": "the base cluster is frozen; write to a clone created with POST /_osmem/clones", ...}, "status": 403}
```

`PUT` on `_mapping`, `_settings` and `_alias` counts as a write.

## The process contract

```
osmem-server [--addr 127.0.0.1:0] [--seed DIR|FILE]... [--freeze] [--no-ja] [--parent-pid N] [--no-stdin-watch]
```

When the server listens it prints exactly one JSON line on stdout and nothing else there:

```json
{"url":"http://127.0.0.1:51132","pid":83231,"version":"2.19.0","japanese":true,"indices":["products"]}
```

Warnings and the exit reason go to stderr. The process exits when its stdin reaches end of file (spawn it with a pipe and it dies with you), on `SIGTERM` or `SIGINT`, or when the process given as `--parent-pid` disappears; `--no-stdin-watch` disables the first rule for interactive use. `--version` prints the build version and exits.

A helper therefore needs four steps: spawn with a piped stdin and `--parent-pid`, read stdout until a line parses as JSON with a `url`, call `POST /_osmem/clones` per test, and close stdin at the end.

## A session with curl

```bash
osmem-server --seed testdata/seed &
# {"url":"http://127.0.0.1:51132",...}
B=http://127.0.0.1:51132

curl -s -XPOST $B/_osmem/clones
# {"id":"c1","url":"http://127.0.0.1:51135"}

curl -s -XPUT $B/products/_doc/9 -H 'content-type: application/json' -d '{}'
# 403 osmem_base_frozen

curl -s -XPUT http://127.0.0.1:51135/products/_doc/9 -H 'content-type: application/json' -d '{"name":"clone only"}'
# 201

curl -s -XDELETE $B/_osmem/clones/c1
```
