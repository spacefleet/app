/// <reference types="vite/client" />

// Populated at runtime by /config.js, served by the Go backend.
// Only pre-approved, non-secret values belong here.
interface AppConfig {
  clerkPublishableKey: string;
}

declare global {
  interface Window {
    appConfig: AppConfig;
  }
}

export {};
