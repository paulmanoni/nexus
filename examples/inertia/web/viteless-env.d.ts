// Ambient declarations so TypeScript resolves this zero-install Inertia project's
// imports with nothing installed. viteless reads viteless.config.ts itself.

declare module "viteless" {
  export function defineConfig<T>(config: T): T
}

declare module "*.vue" {
  import type { DefineComponent } from "vue"
  const component: DefineComponent<{}, {}, any>
  export default component
}

declare module "@inertiajs/vue3" {
  export const createInertiaApp: any
  export const Link: any
  export const router: any
  export function usePage<T = any>(): { props: T }
  export function useForm<T = any>(data?: T): any
}

interface ImportMeta {
  glob: (pattern: string, opts?: { eager?: boolean }) => Record<string, any>
}
