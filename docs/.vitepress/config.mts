import { defineConfig } from 'vitepress'

const repo = 'https://github.com/paulmanoni/nexus'

export default defineConfig({
  title: 'nexus',
  description: 'A Go framework: one typed function served over REST, GraphQL and WebSocket, with dependency injection, an embedded Vite frontend and a live dashboard.',
  base: '/nexus/',
  lang: 'en-US',
  cleanUrls: true,
  lastUpdated: true,
  srcExclude: ['design/**', 'README.md'],
  ignoreDeadLinks: [/^https?:\/\/localhost/],
  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: '/nexus/logo.svg' }],
    ['meta', { name: 'theme-color', content: '#10b981' }],
    ['meta', { property: 'og:title', content: 'nexus — a Go framework' }],
    ['meta', { property: 'og:image', content: '/nexus/banner.svg' }],
  ],
  themeConfig: {
    logo: { light: '/logo.svg', dark: '/logo-dark.svg' },
    nav: [
      { text: 'Guide', link: '/guide/getting-started', activeMatch: '/guide/' },
      { text: 'Reference', link: '/reference/cli', activeMatch: '/reference/' },
      {
        text: 'Links',
        items: [
          { text: 'Changelog', link: `${repo}/blob/main/CHANGELOG.md` },
          { text: 'pkg.go.dev', link: 'https://pkg.go.dev/github.com/paulmanoni/nexus' },
          { text: 'Examples', link: `${repo}/tree/main/examples` },
        ],
      },
    ],
    sidebar: {
      '/guide/': [
        {
          text: 'Introduction',
          items: [
            { text: 'What is nexus?', link: '/guide/' },
            { text: 'Getting started', link: '/guide/getting-started' },
            { text: 'Configuration', link: '/guide/configuration' },
          ],
        },
        {
          text: 'Building an API',
          items: [
            { text: 'Handlers', link: '/guide/handlers' },
            { text: 'REST, GraphQL, WebSocket', link: '/guide/transports' },
            { text: 'Modules & services', link: '/guide/modules' },
            { text: '//@ decorators', link: '/guide/decorators' },
            { text: 'Forms & validation errors', link: '/guide/forms' },
            { text: 'Generated CRUD', link: '/guide/crud' },
          ],
        },
        {
          text: 'Frontend',
          items: [
            { text: 'Vite frontend', link: '/guide/frontend' },
            { text: 'Inertia pages', link: '/guide/inertia' },
            { text: 'Client SDK', link: '/guide/client-sdk' },
          ],
        },
        {
          text: 'Development',
          items: [
            { text: 'The dev loop', link: '/guide/dev-loop' },
            { text: 'Dashboard', link: '/guide/dashboard' },
          ],
        },
        {
          text: 'Features',
          items: [
            { text: 'Databases & caches', link: '/guide/resources' },
            { text: 'Auth', link: '/guide/auth' },
            { text: 'Web security', link: '/guide/security' },
            { text: 'File storage', link: '/guide/storage' },
            { text: 'Mail', link: '/guide/mail' },
            { text: 'Sessions', link: '/guide/sessions' },
            { text: 'Opaque IDs', link: '/guide/maskid' },
            { text: 'Request-scoped values', link: '/guide/scoped' },
            { text: 'Workers & crons', link: '/guide/workers' },
          ],
        },
        {
          text: 'Production',
          items: [
            { text: 'Deployment', link: '/guide/deployment' },
            { text: 'Router & DI backends', link: '/guide/backends' },
          ],
        },
      ],
      '/reference/': [
        {
          text: 'Reference',
          items: [
            { text: 'CLI', link: '/reference/cli' },
            { text: 'nexus.toml', link: '/reference/nexus-toml' },
          ],
        },
      ],
    },
    socialLinks: [{ icon: 'github', link: repo }],
    search: { provider: 'local' },
    editLink: {
      pattern: `${repo}/edit/main/docs/:path`,
      text: 'Edit this page on GitHub',
    },
    outline: { level: [2, 3] },
    footer: {
      message: 'Released under the MIT License.',
      copyright: 'Copyright © Paul Manoni',
    },
  },
})
