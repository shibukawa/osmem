---
title: "Seed data"
description: "Describe the base cluster as a seed directory or bulk file, or seed through the API and freeze."
---

The base cluster is worth describing as files: reviewers can read a mapping change in a pull request, and Go, Node.js, Python and Java suites can share one fixture. osmem reads a seed directory with a fixed layout, or a single bulk file, and can also be seeded through the normal API before the base is frozen.

## Directory layout

Files are applied in this order, sorted by name within each step:

| file | request |
|---|---|
| `<name>.template.json` | `PUT /_index_template/<name>` |
| `<index>.index.json` | `PUT /<index>` (settings, mappings, aliases) |
| `<index>.ndjson` | `POST /<index>/_bulk` |
| `aliases.json` | `POST /_aliases` |

Templates come first so that indices created afterwards pick them up, and aliases last so they can point at populated indices. Any failed bulk item aborts the load with the item's error.

A small catalog:

```
testdata/seed/
├── products.index.json
├── products.ndjson
└── aliases.json
```

```json
{"mappings": {"properties": {
  "name":  {"type": "text", "fields": {"keyword": {"type": "keyword"}}},
  "price": {"type": "double"},
  "tags":  {"type": "keyword"},
  "created": {"type": "date"}
}}}
```

```
{"index": {"_id": "1"}}
{"name": "Red Apple", "price": 1.5, "tags": ["fruit", "red"], "created": "2024-01-05T10:00:00Z"}
{"index": {"_id": "2"}}
{"name": "Banana", "price": 0.5, "tags": ["fruit"], "created": "2024-02-20"}
```

```json
{"actions": [{"add": {"index": "products", "alias": "catalog"}}]}
```

The NDJSON is the body of `_bulk`: an action line followed by a document line. The index defaults to the file name; an action may set `_index` to write elsewhere. Documents whose fields are not in the mapping are mapped dynamically, as on OpenSearch.

## A single bulk file

A path ending in `.ndjson` is loaded as one `_bulk` body. Every action must then carry `_index`; missing indices are created with dynamic mapping.

## Seeding through the API

When the fixture is easier to build in test code, write to the base with any client, then freeze it:

```bash
curl -XPOST http://127.0.0.1:PORT/_osmem/base/freeze
```

Freezing also happens implicitly when the first clone is created. After that, writes to the base return 403 `osmem_base_frozen`, so a suite that seeds in a session fixture and forgets to freeze is still protected once its first test clones.

## Loading

- Go: `c.LoadSeed("testdata/seed")` or `c.LoadSeed("dump.ndjson")`.
- `osmem-server --seed testdata/seed --seed extra.ndjson`, repeatable, applied in order; the language packages pass their `seed` option through.

## Habits that keep fixtures useful

Give documents explicit ids so tests can `GET` them. Keep the base small: a clone re-indexes an index the first time a test writes to it, which takes about 40 µs per document. Dates in `strict_date_optional_time` (`2024-02-20`, `2024-01-05T10:00:00Z`) need no `format` in the mapping. Mappings that use `kuromoji` analyze correctly only with Japanese support enabled (the `ja` import in Go, the default in `osmem-server`); without it they fall back to CJK bigrams and searches for inflected words miss.
