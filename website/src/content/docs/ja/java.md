---
title: "Javaガイド"
description: "osmemのランチャーとJUnit 5拡張を、opensearch-javaと組み合わせて使う。"
---

Javaのパッケージは、`osmem-server`のランチャーとJUnit 5拡張です。サーバーのバイナリは、プラットフォームごとのclassifierを持つ別のアーティファクトとして配布されます。そのため、プロジェクトはランチャーと、テストを実行するプラットフォーム用のバイナリjarの両方に依存します。このページでは、依存関係、拡張、そして他のテストフレームワーク向けのビルダーを説明します。

## 依存関係

```xml
<dependency>
  <groupId>io.github.shibukawa.osmem</groupId>
  <artifactId>osmem</artifactId>
  <version>0.1.0</version>
  <scope>test</scope>
</dependency>
<dependency>
  <groupId>io.github.shibukawa.osmem</groupId>
  <artifactId>osmem-server-binaries</artifactId>
  <version>0.1.0</version>
  <classifier>linux-amd64</classifier>
  <scope>test</scope>
</dependency>
```

classifierは`darwin-arm64`、`linux-amd64`、`linux-arm64`、`windows-amd64`、`windows-arm64`です。Gradleなら`testImplementation("io.github.shibukawa.osmem:osmem-server-binaries:0.1.0:linux-amd64")`。CIと異なるプラットフォームで開発する人は、自分のclassifierも追加するか、`OSMEM_SERVER_BIN`でバイナリを指定してください。

ランチャーに必要なのはJava 17だけです。拡張には、クラスパス上の`junit-jupiter-api`が必要です。

## JUnit 5

`OsmemExtension`はテストクラスごとに1つのサーバーを起動し、各テストメソッドに専用のクローンを渡します。

```java
import io.github.shibukawa.osmem.OsmemClone;
import io.github.shibukawa.osmem.OsmemExtension;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.RegisterExtension;

class ProductSearchTest {
    @RegisterExtension
    static OsmemExtension osmem = OsmemExtension.seed(Path.of("src/test/resources/seed"));

    @Test
    void addsAProduct(OsmemClone clone) {
        OpenSearchClient client = clientFor(clone.url());
        client.index(i -> i.index("products").id("x").document(Map.of("name", "new")));
        assertTrue(client.get(g -> g.index("products").id("x"), Map.class).found());
    }
}
```

`OsmemExtension.seed(...)`は、シード投入後にベースを凍結します。`OsmemExtension.builder(b -> b.seed(...).japanese(false))`なら、すべての起動オプションを指定できます。テストメソッドは`OsmemClone`や`OsmemServer`を引数に取れ、`osmem.clone()`と`osmem.server()`は同じオブジェクトを返します。

`clientFor`は、使うOpenSearchクライアントに合わせて用意します。opensearch-javaとApache HttpClient 5なら次のとおりです。

```java
static OpenSearchClient clientFor(String url) {
    var transport = ApacheHttpClient5TransportBuilder.builder(HttpHost.create(url)).build();
    return new OpenSearchClient(transport);
}
```

## 他のフレームワーク

`OsmemServer`と`OsmemClone`は`AutoCloseable`です。

```java
try (OsmemServer server = OsmemServer.builder().seed(Path.of("seed")).freeze(true).start();
     OsmemClone clone = server.clone()) {
    String body = clone.request("GET", "/products/_count", null);
}
```

ビルダーのオプションは`seed(Path...)`、`freeze(boolean)`、`japanese(boolean)`、`addr(String)`、`binary(Path)`、`startupTimeout(Duration)`です。サーバーやクローンの`request`はJSONリクエストを送り、エラーステータスなら`OsmemException`を投げます。メッセージには、OpenSearchのエラー本文が含まれます。

## バイナリの探し方

ランチャーは、システムプロパティ`osmem.server.bin`、環境変数`OSMEM_SERVER_BIN`、バイナリjar内のクラスパスリソース`osmem/bin/<os>-<arch>/osmem-server`の順に探します。最後の場合は、初回利用時に一時ディレクトリへ展開します。プロセスはJVMとともに終了します。標準入力とJVMのpidを監視しているためです。
