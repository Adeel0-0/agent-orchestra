import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
export default defineConfig({
  plugins: [react()],
  base: "/",
  build: {
    outDir: "../orchestra/app/web/dist",
    emptyOutDir: true,
    sourcemap: false,
  },
});
