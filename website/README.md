# osmem documentation site

Astro + Starlight. English pages live in `src/content/docs/`, Japanese
translations with the same file names in `src/content/docs/ja/`.

```bash
npm install
npm run dev      # http://localhost:4321/osmem/
npm run build
```

Pushing to `main` publishes to https://shibukawa.github.io/osmem/ through
`.github/workflows/docs.yml`.
