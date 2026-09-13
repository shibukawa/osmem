---
id: doc:documentation-plan
type: doc
title: Documentation Plan
---
User documentation is an Astro site (doc:docs-site) with English as the source language and Japanese as a full translation, one page per topic so each language and package can be read on its own.

```yaml
summary:
  audience: developers adding osmem to an existing test suite; assume OpenSearch client knowledge, no bleve knowledge
  languages:
    en: source of truth; written first, token-efficient but readable
    ja: complete translation of every page, same headings and code; not an adaptation
  pages:
    - doc:getting-started
    - doc:go-guide
    - doc:node-guide
    - doc:python-guide
    - doc:java-guide
    - doc:seed-data
    - doc:management-api
    - doc:compatibility
  style:
    - every page opens with what the reader can do after reading it
    - runnable snippets over prose; one snippet per concept
    - state each OpenSearch difference where the reader meets it, not only in doc:compatibility
    - no document-progress narration; sections open on the reader's next question
  readme: README.md stays useful on its own (pkg.go.dev, npm, PyPI render it) with overview, install and short examples; duplication with the site is accepted; links to the site for depth
  maintenance: docs change in the same commit as behaviour; catalog concepts remain the spec, site pages the prose
  references:
    - vision:osmem
    - policy:fidelity-first
    - doc:docs-site
```
