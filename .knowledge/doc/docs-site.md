---
id: doc:docs-site
type: doc
title: Documentation Site (Astro)
---
The documentation site is built with Astro and its Starlight docs theme, lives in the repository, and is published to GitHub Pages by CI.

```yaml
summary:
  status: implemented 2026-09-13 (astro 7.3, starlight 0.42; 9 pages + splash index in en and ja; build 21 pages)
  stack: Astro + @astrojs/starlight (sidebar, i18n, Pagefind search, dark mode, Markdown/MDX content); homepage-only custom CSS for metric cards and accessible bar charts
  location: website/ in this repository (package.json, astro.config.mjs, src/content/docs/)
  content_layout:
    src/content/docs/<page>.md or .mdx: English (root locale)
    src/content/docs/ja/<page>.md or .mdx: Japanese
    frontmatter: title, description only; Starlight renders headings into the page TOC
  navigation:
    sidebar: Getting started; Performance; Guides (Go, Node.js, Python, Java); Reference (Seed data, Management API, Compatibility)
    sidebar labels translated via Starlight i18n
  homepage:
    hero: per-test isolated search state without a fresh container per test
    proof: local averages for Japanese-enabled startup, RSS, linked Go binary delta, Docker Hub compressed image size
    charts: startup and resident-memory comparison for osmem, Docker OpenSearch, and Testcontainers; Devbox environment setup shown separately
    detail: doc:performance owns workload definitions, caveats, and full measurements
  hosting:
    url: https://shibukawa.github.io/osmem/ (base path /osmem); no custom domain
    deploy: .github/workflows/docs.yml on push to main touching website/ (withastro/action@v6 build, actions/deploy-pages@v5); PRs build only
  local: npm run dev in website/ (http://localhost:4321/osmem/); npm run build must pass
  gotchas: quote frontmatter values containing ':'; npm 11 needs `npm install-scripts approve esbuild sharp`; links between pages are relative (../go/) so they work under /osmem and /osmem/ja
  code_samples: kept in sync with packages by copying from tests where possible; snippets are not executed by the site build
  versioning: single version (latest main); no version switcher until the API changes incompatibly
  readme_policy: README.md keeps overview, install and short per-language examples even if they duplicate the site, because pkg.go.dev and package registries render it; it links to the site for everything else
  decided: 2026-09-13 (github.io domain, README duplication allowed)
  references:
    - doc:documentation-plan
```
