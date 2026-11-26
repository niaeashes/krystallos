import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// In development the UI is served by Vite and talks to the Go server through
// this proxy, so the browser sees one origin: http://localhost:5173. Passkeys
// are bound to that origin (see `kry serve --origin`).
const server = process.env.KRYSTALLOS_SERVER ?? 'http://127.0.0.1:3000'

export default defineConfig({
  plugins: [react()],
  server: {
    host: 'localhost',
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': server,
      '/git': server,
    },
  },
})
