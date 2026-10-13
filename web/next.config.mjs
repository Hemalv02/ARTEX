import { fileURLToPath } from "node:url";

// Static export: `NEXT_EXPORT=1 next build` produces a pure-static directory
// under web/out, ready to drop into an nginx web root. In dev (next dev) this
// variable is unset, so /api rewrites and HMR are preserved.
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel demo: the whole site runs against mock data, no backend, no /api rewrite.
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // Avoid a parent lockfile affecting root detection and asset path generation.
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // Allow access to dev assets (HMR) from LAN IPs — adjust as needed.
  // During dev, allow any IPv4 source to access /_next/* and HMR (LAN IP changes are fine).
  // Note: Next forbids a bare "*" for security; segment wildcards are required.
  // "*.*.*.*" matches any IPv4.
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // Pure-static export: no Node runtime; images are not optimized; each route emits <route>/index.html.
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock demo: no backend, no /api rewrite.
          images: { unoptimized: true },
        }
      : {
          // Dev: proxy /api/* to the Go backend (default :8787, override with AUTOPENTEST_API).
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
