import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    projects: ["sdk/typescript/vitest.config.ts", "plugins/*/vitest.config.ts"],
  },
});
