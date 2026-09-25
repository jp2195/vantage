import { defineConfig } from '@hey-api/openapi-ts'

export default defineConfig({
  // The contract in the repository, not a URL. Generating from a running
  // daemon would make the client depend on which daemon happened to be up.
  input: '../api/openapi.yaml',
  // No `format` option: this package has no prettier dependency and none is
  // added just to restyle output nobody hand-edits. With none configured
  // openapi-ts runs no formatter at all, so what is committed under
  // src/api/generated/ is the generator's own default emit -- which is why
  // regenerating is a no-op on any machine, prettier installed or not, and
  // why CI's drift check can compare the committed files byte for byte.
  //
  // Do not set `format: 'prettier'` without also adding the dependency.
  // openapi-ts announces the formatter and then shells out with
  // cross-spawn's sync() and never looks at the result, so a missing binary
  // formats nothing while still printing "Running Prettier" and exiting 0 --
  // and the drift check would then fail on whichever machine does have
  // prettier on its PATH.
  output: { path: './src/api/generated' },
  plugins: ['@hey-api/client-fetch'],
})
