---
id: doc:docs-site
type: doc
title: Documentation Site (Astro)
---
The documentation site is built with Astro and its Starlight docs theme, lives in the repository, and is published to GitHub Pages by CI.

```yaml
summary:
  status: implemented 2026-09-13 (astro 7.3, starlight 0.42; 8 pages + splash index in en and ja; build 19 pages)
  stack: Astro + @astrojs/starlight (sidebar, i18n, Pagefind search, dark mode, Markdown/MDX content); no custom design work
  location: website/ in this repository (package.json, astro.config.mjs, src/content/docs/)
  content_layout:
    src/content/docs/<page>.md: English (root locale)
    src/content/docs/ja/<page>.md: Japanese
    frontmatter: title, description only; Starlight renders headings into the page TOC
  navigation:
    sidebar: Getting started; Guides (Go, Node.js, Python, Java); Reference (Seed data, Management API, Compatibility)
    sidebar labels translated via Starlight i18n
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
