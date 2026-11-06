/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** The release stamp baked in by the build (docker/Dockerfile.web). */
  readonly VITE_APP_VERSION?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
