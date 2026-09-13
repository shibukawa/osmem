---
id: doc:java-guide
type: doc
title: Java Guide Page
---
Using io.github.shibukawa.osmem:osmem with JUnit 5.

```yaml
summary:
  sections:
    - tabbed Gradle Java, Gradle Kotlin, and Maven coordinates incl. osmem-server-binaries classifier
    - include official opensearch-java client and Apache HttpClient 5 transport
    - direct server startup, schema/seed via REST, and official-client query
    - OsmemExtension via @RegisterExtension, OsmemClone parameter per test
    - class-level base reuse; use per-test writable forks for document, mapping, or index lifecycle mutations; fresh server when setup differs
    - builder options (seed, freeze, japanese, binary, startupTimeout)
    - opensearch-java client example
    - binary resolution order (system property, env, classpath resource)
  references:
    - api:server-cli
    - api:clone-admin-api
```
