---
title: "管理API"
description: "/_osmemエンドポイントとosmem-serverのプロセス規約。独自のヘルパーを書くための仕様。"
---

各言語のパッケージは薄く作られています。どれも`osmem-server`を起動して1行を読み、`/_osmem`以下のいくつかのエンドポイントを呼ぶだけです。このページは、それらが依存している規約です。別の言語やテストフレームワーク向けのヘルパーを書く人に向けています。

## エンドポイント

| リクエスト | 効果 | レスポンス |
|---|---|---|
| `GET /_osmem` | サーバー情報 | `{"version","frozen","clones"}` |
| `GET /_osmem/base` | ベースの状態 | `{"frozen","clones","indices"}` |
| `POST /_osmem/base/freeze` | ベースへの書き込みを拒否する | `{"acknowledged":true,"frozen":true}` |
| `POST /_osmem/base/unfreeze` | 書き込みを再び許可する | `{"acknowledged":true,"frozen":false}` |
| `POST /_osmem/clones` | ベースをクローンし(ベースは凍結される)、新しいループバックのポートで公開する | `201 {"id","url"}` |
| `GET /_osmem/clones` | クローンの一覧 | `{"clones":[{"id","url","created"}]}` |
| `GET /_osmem/clones/{id}` | 1つのクローン | `{"id","url","created"}`または404 |
| `DELETE /_osmem/clones/{id}` | クローンとそのポートを閉じる | `{"acknowledged":true}`または404 |
| `DELETE /_osmem/clones` | すべてのクローンを閉じる | `{"acknowledged":true,"closed":n}` |

クローンは専用のポートを持つ完全なクラスタで、通常のOpenSearch APIで使います。クローンを閉じてもベースには影響しません。サーバーを止めると、それが作ったクローンはすべて閉じられます。

## 凍結中のベースが受け付けるもの

`GET`、`HEAD`、`OPTIONS`は常に通ります。`POST`、`PUT`、`DELETE`のうち、パスに`_search`、`_count`、`_msearch`、`_mget`、`_analyze`、`_explain`、`_validate`、`_field_caps`、`_refresh`、`_flush`、`_forcemerge`、`_cache`、`_stats`、`_cat`、`_cluster`、`_nodes`を含むものも通ります。scrollとpoint in timeの操作もこれに含まれます。それ以外は次のレスポンスになります。

```json
{"error": {"type": "osmem_base_frozen", "reason": "the base cluster is frozen; write to a clone created with POST /_osmem/clones", ...}, "status": 403}
```

`_mapping`、`_settings`、`_alias`への`PUT`は、書き込みとして扱われます。

## プロセスの規約

```
osmem-server [--addr 127.0.0.1:0] [--seed DIR|FILE]... [--freeze] [--no-ja] [--parent-pid N] [--no-stdin-watch]
```

サーバーはlistenを始めると、標準出力にJSONをちょうど1行出力します。標準出力には、それ以外何も出しません。

```json
{"url":"http://127.0.0.1:51132","pid":83231,"version":"2.19.0","japanese":true,"indices":["products"]}
```

警告と終了理由は標準エラーに出ます。プロセスが終了するのは、標準入力がEOFに達したとき(パイプで起動すれば、呼び出し元とともに終了します)、`SIGTERM`か`SIGINT`を受けたとき、`--parent-pid`で指定したプロセスが消えたときです。対話的に使うなら、`--no-stdin-watch`で1つ目の条件を無効にできます。`--version`は、ビルドのバージョンを表示して終了します。

つまり、ヘルパーに必要な手順は4つです。標準入力をパイプにし、`--parent-pid`を付けて起動する。`url`を含むJSONとして解釈できる行が来るまで、標準出力を読む。テストごとに`POST /_osmem/clones`を呼ぶ。最後に標準入力を閉じる。

## curlでのセッション例

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
