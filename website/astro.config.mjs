// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// Published to https://shibukawa.github.io/osmem/ by .github/workflows/docs.yml.
export default defineConfig({
  site: 'https://shibukawa.github.io',
  base: '/osmem',
  trailingSlash: 'always',
  integrations: [
    starlight({
      title: 'osmem',
      description: 'In-memory OpenSearch-compatible server for tests',
      customCss: ['./src/styles/home.css'],
      defaultLocale: 'root',
      locales: {
        root: { label: 'English', lang: 'en' },
        ja: { label: '日本語', lang: 'ja' },
      },
      social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/shibukawa/osmem' }],
      editLink: { baseUrl: 'https://github.com/shibukawa/osmem/edit/main/website/' },
      sidebar: [
        { slug: 'getting-started' },
        { slug: 'performance' },
        {
          label: 'Guides',
          translations: { ja: 'ガイド' },
          items: [{ slug: 'go' }, { slug: 'node' }, { slug: 'python' }, { slug: 'java' }],
        },
        {
          label: 'Reference',
          translations: { ja: 'リファレンス' },
          items: [{ slug: 'seed-data' }, { slug: 'management-api' }, { slug: 'compatibility' }],
        },
      ],
    }),
  ],
});
