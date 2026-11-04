// @ts-check
// Note: type annotations allow type checking and IDEs autocompletion.

const {themes: prismThemes} = require('prism-react-renderer');

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'Orka',
  tagline: 'Kubernetes-native AI task orchestration',
  url: 'https://orka-agents.github.io',
  baseUrl: '/orka/',
  favicon: 'img/favicon.png',
  organizationName: 'orka-agents',
  projectName: 'orka',
  trailingSlash: false,

  onBrokenLinks: 'throw',
  onBrokenAnchors: 'throw',
  markdown: {
    mermaid: true,
    hooks: {
      onBrokenMarkdownLinks: 'warn',
    },
  },

  themes: ['@docusaurus/theme-mermaid'],

  // Self-hosted Geist and Geist Mono (no runtime font CDN).
  clientModules: [require.resolve('./src/clientModules/fonts.js')],

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          path: 'docs',
          routeBasePath: 'docs',
          sidebarPath: require.resolve('./sidebars.js'),
          editUrl: ({docPath}) =>
            `https://github.com/orka-agents/orka/edit/main/website/docs/${docPath}`,
        },
        blog: false,
        theme: {
          customCss: require.resolve('./src/css/custom.css'),
        },
      }),
    ],
  ],

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      colorMode: {
        respectPrefersColorScheme: true,
      },
      mermaid: {
        theme: {light: 'neutral', dark: 'dark'},
      },
      navbar: {
        title: 'Orka',
        logo: {
          alt: '',
          src: 'img/orka-mark.png',
          srcDark: 'img/orka-mark-dark.png',
          width: 30,
          height: 30,
        },
        items: [
          {
            type: 'docSidebar',
            sidebarId: 'tutorialSidebar',
            position: 'left',
            label: 'Docs',
          },
          {
            to: '/docs/demos',
            label: 'Demos',
            position: 'left',
          },
          {
            to: 'pathname:///demo/',
            label: 'Dashboard demo',
            position: 'left',
          },
          {
            href: 'https://github.com/orka-agents/orka',
            label: 'GitHub',
            position: 'right',
          },
        ],
      },
      footer: {
        style: 'dark',
        logo: {
          alt: 'Orka',
          src: 'img/orka-mark-dark.png',
          width: 44,
          height: 44,
        },
        links: [
          {
            title: 'Docs',
            items: [
              {
                label: 'What is Orka?',
                to: '/docs',
              },
              {
                label: 'Getting started',
                to: '/docs/getting-started',
              },
              {
                label: 'Resource reference',
                to: '/docs/resources',
              },
              {
                label: 'API reference',
                to: '/docs/api-reference',
              },
            ],
          },
          {
            title: 'Try it',
            items: [
              {
                label: 'Interactive dashboard demo',
                to: 'pathname:///demo/',
              },
              {
                label: 'Demo videos',
                to: '/docs/demos',
              },
              {
                label: 'Examples',
                href: 'https://github.com/orka-agents/orka/tree/main/examples',
              },
            ],
          },
          {
            title: 'Community',
            items: [
              {
                label: 'GitHub',
                href: 'https://github.com/orka-agents/orka',
              },
              {
                label: 'Issues',
                href: 'https://github.com/orka-agents/orka/issues',
              },
              {
                label: 'Contributing',
                href: 'https://github.com/orka-agents/orka/blob/main/CONTRIBUTING.md',
              },
            ],
          },
        ],
        copyright: `Copyright © ${new Date().getFullYear()} Orka contributors.`,
      },
      // Code blocks stay dark in both colour modes; custom.css sets their ground.
      prism: {
        theme: prismThemes.vsDark,
        darkTheme: prismThemes.vsDark,
      },
    }),
};

module.exports = config;
