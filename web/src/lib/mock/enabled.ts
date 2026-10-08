// Mock toggle. A public variable injected at build time (the NEXT_PUBLIC_ prefix makes it browser-readable).
// Set NEXT_PUBLIC_MOCK=1 on Vercel to run the whole site on mock with no backend.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
