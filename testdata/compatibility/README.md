# OpenSearch 3.8.0 compatibility probes

`probes.json` contains **32 independent scenarios** verified against a running OpenSearch **3.8.0** server on 2026-09-15. Each scenario includes setup requests, a checked request, expected HTTP status, exact JSON Pointer assertions, and optional follow-up requests whose statuses check side effects. The fixed osmem engine passes the same assertions. This is targeted coverage, not full OpenSearch 3.8 compatibility certification; error messages, timing, and unasserted response fields may differ.

## Run osmem regression tests

From the repository root:

```sh
go test -run '^TestCompatibilityProbes$' -count=1 -v .
```

These cases now run in ordinary `go test ./...` as regression tests. Each scenario gets a fresh in-process osmem cluster. Go's `json.Number` preserves version precision. To save actual responses, add `OSMEM_COMPAT_REPORT=/tmp/osmem-observations.json` to the command. A setup failure is an error, not a compatibility finding; it produces no observation for that scenario. The report path is overwritten.

## Replay against OpenSearch

Start a disposable official server, bound to localhost. The image used in this audit was `opensearchproject/opensearch:3.8.0`, digest `sha256:bcc1797519726ceb6d651d4a3e60b7c30da91793914a8dfe75fd441d4f641509`.

```sh
docker run -d --name osmem-compat-380 \
  -p 127.0.0.1::9200 \
  -e discovery.type=single-node \
  -e DISABLE_SECURITY_PLUGIN=true \
  -e DISABLE_INSTALL_DEMO_CONFIG=true \
  -e 'OPENSEARCH_JAVA_OPTS=-Xms512m -Xmx512m' \
  opensearchproject/opensearch:3.8.0
docker port osmem-compat-380 9200
```

Wait until `GET /` responds, then use the host port shown above:

```sh
python3 testdata/compatibility/run_opensearch.py \
  --url http://127.0.0.1:PORT \
  --report /tmp/opensearch-3.8.0-observations.json
docker rm -f osmem-compat-380
```

The standard-library Python runner verifies server version `3.8.0`, preserves integer precision, substitutes a unique resource prefix per scenario, and deletes only those exact index/template names afterward. It reports assertion, setup/transport, and cleanup errors with a nonzero exit status. Use a disposable cluster: unrelated templates or cluster settings can affect results. The runner does not provide authentication/TLS configuration for production endpoints.

## Evidence and provenance

- `observations-opensearch-3.8.0.json`: real server responses, including root version/build information; **32 passing cases**, no setup/transport or cleanup errors. Server build hash: `e5a3c5691be87af6c12dbe3e158c59c04ee72973`.
- `observations-osmem-fixed.json`: responses after the engine changes; **32 passing cases**. Generated IDs and other unasserted runtime fields are incidental.
- `observations-v0.1.3.json`: historical pre-fix osmem observations at engine commit `2b96669966be7155de5baec6a7dfbdee5153b8ea`. Only the original 13 cases are included: 12 differences, 1 passing control. This file used source-derived OpenSearch 2.19.1 expectations and is retained as before-fix evidence.

`resolve-open-control` adapts the open-index portion of upstream [`10_basic_resolve_index.yml`](https://github.com/opensearch-project/OpenSearch/blob/2.19.1/rest-api-spec/src/main/resources/rest-api-spec/test/indices.resolve_index/10_basic_resolve_index.yml), with a scoped wildcard instead of `*`. Closed-index setup and the full YAML suite are not imported. Other probes were constructed from upstream code, with their original source links retained. All current expectations were subsequently checked against 3.8.0, including three corrected assumptions:

- JSON fractional Bulk version `1.5` succeeds with version `1`; overflow beyond signed 64-bit range is rejected.
- `expand_wildcards=none` or `open,none` returns empty arrays, whereas `none,open` enables open indices. `hidden` alone does not enable open indices.
- OpenSearch 3.8's [template overlap check](https://github.com/opensearch-project/OpenSearch/blob/3.8.0/server/src/main/java/org/opensearch/cluster/metadata/MetadataIndexTemplateService.java#L870) uses stripped-pattern matching. `logs*` and `*2026` under the same prefix may coexist even at equal priority. The engine follows that behavior instead of rejecting every theoretical glob intersection.
