import { defineConfig } from "tsdown";

export default defineConfig({
  entry: {
    index: "src/index.ts",
    // CopilotKit React bindings live on their own entry so the agent can be
    // used without React (another AG-UI client, or CopilotKit's other SDKs).
    react: "src/react.ts",
  },
  format: ["esm", "cjs"],
  dts: true,
  sourcemap: true,
  clean: true,
  // Every runtime dependency is a peer: the app's own copies must be used, or
  // CopilotKit would see a second HttpAgent class and a second React.
  external: [/^@ag-ui\//, /^@copilotkit\//, "react", "rxjs"],
});
