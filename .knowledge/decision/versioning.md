---
id: decision:versioning
type: decision
title: Published Version Scheme
---
The planned shared version format for published osmem language packages is `1.<opensearch-major>.<repository-version>`. The leading `1` is fixed; the second segment tracks the supported OpenSearch major, currently `9`; the final segment is this repository's own release version, not the OpenSearch minor or patch version. Thus the current series is `1.9.y`. Apply this scheme to the language artifacts in decision:bundled-binaries.
