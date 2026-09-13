# osmem for Java

Launcher and JUnit 5 extension for `osmem-server`, the in-memory
OpenSearch-compatible server for tests.

```java
@RegisterExtension
static OsmemExtension osmem = OsmemExtension.seed(Path.of("src/test/resources/seed"));

@Test
void search(OsmemClone clone) {
    // any OpenSearch client pointed at clone.url(); the clone is discarded after the test
}
```

Dependencies (Maven):

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
  <classifier>linux-amd64</classifier> <!-- darwin-arm64, linux-arm64, windows-amd64, windows-arm64 -->
  <scope>test</scope>
</dependency>
```

The binary jar contains `osmem/bin/<os>-<arch>/osmem-server`; it is
extracted to a temp directory on first use. `-Dosmem.server.bin=...` or
`OSMEM_SERVER_BIN` point at a locally built binary instead.

Layout: `osmem/` (launcher + extension), `binaries/` (pom-only module
that attaches the classifier jars produced by
`scripts/build-java-binaries.sh` during `mvn -Prelease deploy`). The
launcher has no dependencies beyond the JDK (17+); the JUnit extension
needs junit-jupiter-api on the classpath.

Full guide: [English](https://shibukawa.github.io/osmem/java/) · [日本語](https://shibukawa.github.io/osmem/ja/java/)
