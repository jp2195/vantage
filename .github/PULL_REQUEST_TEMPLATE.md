## What changed and why

<!-- What this does, what you measured, and what you ruled out. -->

## Checklist

- [ ] Each new or changed test failed before the fix. I watched it go red.
- [ ] I broke the code on purpose (reverted the fix, flipped the comparison, deleted the clause) and a test caught it.
- [ ] Any new fixtures were captured from a real endpoint or router, not written by hand.
- [ ] `go test ./...` passes, and for UI changes `cd ui && npx vitest run && npx vue-tsc --noEmit` does too.

See [`CONTRIBUTING.md`](https://github.com/jp2195/vantage/blob/main/CONTRIBUTING.md) for why each of these is here.
