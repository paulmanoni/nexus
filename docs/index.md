---
layout: home

hero:
  name: nexus
  text: One Go function. Every transport.
  tagline: Write a plain typed function and serve it over REST, GraphQL and WebSocket, with dependency injection, an embedded Vite frontend and a live dashboard. It all ships as one binary.
  image:
    light: /logo.svg
    dark: /logo-dark.svg
    alt: nexus
  actions:
    - theme: brand
      text: Get started
      link: /guide/getting-started
    - theme: alt
      text: What is nexus?
      link: /guide/
    - theme: alt
      text: GitHub
      link: https://github.com/paulmanoni/nexus

features:
  - icon: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="26" height="26" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><rect x="16" y="16" width="6" height="6" rx="1"/><rect x="2" y="16" width="6" height="6" rx="1"/><rect x="9" y="2" width="6" height="6" rx="1"/><path d="M5 16v-3a1 1 0 0 1 1-1h12a1 1 0 0 1 1 1v3"/><path d="M12 12V8"/></svg>'
    title: One signature, three transports
    details: Register the same function with AsRest, AsQuery or AsWS. Arguments, validation, auth gates and tracing behave the same on every transport.
    link: /guide/handlers
    linkText: Handlers
  - icon: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="26" height="26" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><rect width="7" height="7" x="14" y="3" rx="1"/><path d="M10 21V8a1 1 0 0 0-1-1H4a1 1 0 0 0-1 1v12a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-5a1 1 0 0 0-1-1H3"/></svg>'
    title: Dependency injection built in
    details: Handlers and services take their dependencies as parameters. A small built-in container wires them; uber/fx is available as an option.
    link: /guide/modules
    linkText: Modules & services
  - icon: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="26" height="26" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><path d="M4 14a1 1 0 0 1-.78-1.63l9.9-10.2a.5.5 0 0 1 .86.46l-1.92 6.02A1 1 0 0 0 13 10h7a1 1 0 0 1 .78 1.63l-9.9 10.2a.5.5 0 0 1-.86-.46l1.92-6.02A1 1 0 0 0 11 14z"/></svg>'
    title: Vite frontend, embedded
    details: An ordinary Vite project under web/ with HMR in development. nexus build embeds the output, so you still deploy one Go binary. Inertia pages are supported.
    link: /guide/frontend
    linkText: Frontend
  - icon: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="26" height="26" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><path d="M22 12h-2.48a2 2 0 0 0-1.93 1.46l-2.35 8.36a.25.25 0 0 1-.48 0L9.24 2.18a.25.25 0 0 0-.48 0l-2.35 8.36A2 2 0 0 1 4.49 12H2"/></svg>'
    title: Live dashboard
    details: /__nexus draws your modules, endpoints, resources, workers and crons, and streams traffic over a WebSocket. It is locked down by default in production.
    link: /guide/dashboard
    linkText: Dashboard
  - icon: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="26" height="26" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"/><path d="m9 12 2 2 4-4"/></svg>'
    title: Auth, security and sessions
    details: Cross-transport auth gates, pluggable password hashing and login backends, an OAuth2 server, CSRF and security headers, and server-side sessions.
    link: /guide/auth
    linkText: Auth
  - icon: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="26" height="26" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/></svg>'
    title: A fast dev loop
    details: nexus dev compiles the next build while the current one keeps serving, then swaps in about 20ms. A broken save never takes the app down.
    link: /guide/dev-loop
    linkText: The dev loop
---
