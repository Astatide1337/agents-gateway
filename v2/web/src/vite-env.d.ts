/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_AGW_API_URL?: string;
  /** Set to demo only for local fixture/demo work. Unset means live API mode. */
  readonly VITE_AGW_MODE?: 'live' | 'demo' | string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
