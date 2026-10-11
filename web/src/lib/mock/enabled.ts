// Mock toggle. Build-time public env var (the NEXT_PUBLIC_ prefix is required for browser access).
// Set NEXT_PUBLIC_MOCK=1 on Vercel and the whole site runs against mocks, no backend required.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
