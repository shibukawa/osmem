---
id: requirement:multi-language-clients
type: requirement
title: Java, Python and Node.js Support
---
Test suites in Java, Python and Node.js use osmem through the same OpenSearch REST API by running the Go server binary as a subprocess.

```yaml
summary:
  transport: HTTP on a loopback port; official OpenSearch clients need no changes
  decision: decision:subprocess-for-other-languages
  needs:
    - single static binary per OS/arch (`osmem-server`), no runtime dependencies
    - startup contract: prints or writes the bound address; exits when parent exits or stdin closes
    - seed loading without Go code: api:server-cli
    - clone/dispose from the client side: api:clone-admin-api
    - thin helper packages: implemented under packages/ (node: @osmem/core + @osmem/<platform>; python: osmem with pytest plugin; java: dev.osmem:osmem with OsmemExtension + osmem-server-binaries classifier jars)
  verification: node and python helpers tested against the built binary; java launcher compiled and smoke-tested with javac, JUnit extension not compiled (no Maven/JUnit jars on the dev machine)
  distribution: bundled per ecosystem (decision:bundled-binaries)
  references:
    - flow:subprocess-test-flow
    - requirement:seed-once-reuse
```
