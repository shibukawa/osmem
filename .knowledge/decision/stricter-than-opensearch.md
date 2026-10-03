---
id: decision:stricter-than-opensearch
type: decision
title: Reject Non-Portable Inputs
---
osmem may reject input that OpenSearch core accepts when that input is deprecated in core or fails on a major AWS deployment target (system:opensearch-serverless). A test passing on osmem must then pass on every target, which extends policy:fidelity-first in the safe direction.

```yaml
decision:
  date: 2026-10-03
  trigger: dot-prefixed index names work on managed domain, fail on Serverless and in its data access policies
  applied_by_default: rule:portable-index-name (no-leading-dot, alias-lowercase, charset warning)
  applied_in_serverless_mode: requirement:serverless-portability
admission_criteria:  # all required before adding a new default stricter check
  - core accepts only with a deprecation warning, OR a documented/observed AWS target rejects it
  - realistic test code could hit it unintentionally
  - error uses the OpenSearch error type core would use for the nearest real rejection
  - weak evidence (indirect docs) -> warning by default, rejection only in serverless mode
non_goals:
  - emulating Serverless-only API surface by default
  - rejecting input core and all AWS targets accept
escape_hatch: osmem.WithDotNames() / --allow-dot-names for fixtures reproducing dot-prefixed indices (.kibana etc.)
consequence:
  - divergences listed in doc:compatibility (website compatibility page, README)
  - resolve_compatibility_test.go dot-index assertions run on a WithDotNames cluster
```
