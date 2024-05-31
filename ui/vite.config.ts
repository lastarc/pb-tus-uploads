import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import { viteSingleFile } from "vite-plugin-singlefile"
// import { resolve } from 'path'

// https://vitejs.dev/config/
export default defineConfig({
  build: {
    rollupOptions: {
      // input: {
      //   main: resolve(__dirname, 'index.html'),
      // },
      // output: {
      //   dir: resolve(__dirname, '..', 'pb_public')
      // }
    }
  },
  plugins: [svelte(), viteSingleFile()],
  server: {
    proxy: {
      '^/(uploads|accref|api|_)': 'http://localhost:8090'
    }
  }
})
