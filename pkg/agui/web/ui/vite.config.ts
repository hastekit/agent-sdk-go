import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { resolve } from "node:path";

// Builds the CopilotKit chat app into ../static, which the Go package
// embeds via go:embed. The build output (index.html + assets/) is
// committed so `go build` works without a Node toolchain — only
// rebuild when the UI source changes (see ui/README.md).
//
//   base: "./"        → assets referenced relatively, so the embedded
//                       UI works whether served at "/" or behind a
//                       sub-path.
//   emptyOutDir: true → static/ is wiped and regenerated each build;
//                       basic.html survives because it lives in
//                       public/ and Vite copies public/* into the
//                       output.
export default defineConfig({
  plugins: [react()],
  base: "./",
  resolve: {
    alias: {
      // CopilotKit v2 imports `streamdown` for markdown rendering,
      // which drags in Shiki (every language grammar) + Mermaid +
      // Cytoscape — ~14MB of chunks that would be embedded into the
      // Go binary. Swap it for a lightweight react-markdown shim so
      // that weight never enters the build. See src/streamdown-lite.tsx.
      streamdown: resolve(__dirname, "src/streamdown-lite.tsx"),
      // CopilotKit dynamically imports katex's CSS for LaTeX math
      // styling, which pulls in ~1MB of KaTeX fonts. Our shim doesn't
      // render math; stub the CSS out.
      "katex/dist/katex.min.css": resolve(__dirname, "src/empty.css"),
      // The web-inspector dev console (~850KB) is loaded only when
      // showDevConsole is true; we never enable it. Stub it out.
      "@copilotkit/web-inspector": resolve(__dirname, "src/web-inspector-stub.ts"),
    },
  },
  build: {
    outDir: resolve(__dirname, "../static"),
    emptyOutDir: true,
  },
  server: {
    port: 5180,
    // Dev-server proxy so `pnpm dev` hits a locally running Go server
    // (web.Serve / agui.NewHandler) without CORS.
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
});
