import { defineConfig } from "@playwright/test";
export default defineConfig({
  testDir: "./e2e",
  workers: 1,
  timeout: 45000,
  use: {
    channel: "msedge",
    headless: true,
    viewport: { width: 1440, height: 940 },
  },
  reporter: "list",
});
