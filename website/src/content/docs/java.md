---
title: "Java guide"
description: "Use the osmem launcher and JUnit 5 extension with opensearch-java."
---

The Java package is a launcher for `osmem-server` plus a JUnit 5 extension. The server binary is shipped as a separate artifact with one classifier per platform, so a project depends on the launcher and on the binaries jar for the platform its tests run on. This page covers the dependencies, the extension, and the builder for other test frameworks.

## Dependencies

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

Classifiers: `darwin-arm64`, `linux-amd64`, `linux-arm64`, `windows-amd64`, `windows-arm64`. Gradle: `testImplementation("io.github.shibukawa.osmem:osmem-server-binaries:0.1.0:linux-amd64")`. Developers on a different platform than CI add their own classifier too, or point `OSMEM_SERVER_BIN` at a binary.

The launcher needs Java 17 and nothing else; the extension needs `junit-jupiter-api` on the classpath.

## JUnit 5

`OsmemExtension` starts one server per test class and hands each test method its own clone:

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

`OsmemExtension.seed(...)` freezes the base after seeding. `OsmemExtension.builder(b -> b.seed(...).japanese(false))` exposes every start option. Test methods may declare `OsmemClone` or `OsmemServer` parameters; `osmem.clone()` and `osmem.server()` return the same objects.

`clientFor` is whatever your OpenSearch client needs; with opensearch-java and Apache HttpClient 5:

```java
static OpenSearchClient clientFor(String url) {
    var transport = ApacheHttpClient5TransportBuilder.builder(HttpHost.create(url)).build();
    return new OpenSearchClient(transport);
}
```

## Other frameworks

`OsmemServer` and `OsmemClone` are `AutoCloseable`:

```java
try (OsmemServer server = OsmemServer.builder().seed(Path.of("seed")).freeze(true).start();
     OsmemClone clone = server.clone()) {
    String body = clone.request("GET", "/products/_count", null);
}
```

Builder options: `seed(Path...)`, `freeze(boolean)`, `japanese(boolean)`, `addr(String)`, `binary(Path)`, `startupTimeout(Duration)`. `request` on a server or clone sends a JSON request and throws `OsmemException` on an error status; the message contains OpenSearch's error body.

## Where the binary comes from

The launcher checks, in order, the system property `osmem.server.bin`, the environment variable `OSMEM_SERVER_BIN`, and the classpath resource `osmem/bin/<os>-<arch>/osmem-server` from the binaries jar, which it extracts to a temporary directory on first use. The process exits with the JVM: it watches its stdin and the JVM's pid.
