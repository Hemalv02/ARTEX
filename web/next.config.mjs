import { fileURLToPath } from "node:url";

// Static export: `NEXT_EXPORT=1 next build` emits a fully static directory to web/out,
// which can be dropped straight into an nginx web root. Development (next dev) leaves this
// variable unset, keeping the /api reverse proxy and hot reload.
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel demo: the whole site runs on mocks, with no backend and no need for the /api proxy.
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // Prevent a lockfile in a parent directory from affecting root inference and asset path generation.
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // Allow access to dev resources (HMR) from LAN IPs; add or remove as needed.
  // During dev, open up /_next/* and HMR to any IPv4 origin (unaffected by changing LAN IPs).
  // Note: Next forbids a bare "*" for security, so use a segmented wildcard; "*.*.*.*" matches any IPv4.
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // Pure static export: no Node runtime; images are not optimized; each route emits <route>/index.html.
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock demo: no backend, so the /api proxy is not needed.
          images: { unoptimized: true },
        }
      : {
          // Development: reverse-proxy /api/* to the Go backend (defaults to :8787, overridable via AUTOPENTEST_API).
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
