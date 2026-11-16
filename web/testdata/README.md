# web/testdata: the JavaScript checks

The `*_test.mjs` files are run by the Go tests of `web/` (`go test ./web/...`),
through `runNode` / `runNodeIO` in `web/nodetest_test.go`, never with a bare
`node` (`TestNoBareNodeRuns` holds the test files to that).

Every run preloads `safe_assert.mjs`. A failed `assert.equal` / `deepEqual`
of two stand-in DOM nodes makes node print both whole pages and diff them,
which once took some 236 GB (2026-11-11). With the preload such a failure is
a short message at once; pass or fail stays node's own answer. The Go
helpers also cap the process's memory (2 GB, Windows) and time (2 minutes).

To run one by hand, from `web/` (Git Bash), always with the preload, a heap
cap and a timeout:

```sh
timeout 120 node --import ./testdata/safe_assert.mjs --max-old-space-size=1024 \
  testdata/ui320_test.mjs "file:///$(pwd -W)/"
```

The file URL of the `web/` directory is the argument of most of them (see
the Go test that runs each; `retry_model_test.mjs` takes none,
`batch_default_test.mjs` also its cases as JSON, `provider_origin_test.mjs`
its pairs on stdin). A new test file: run it with `runNode`, and compare
stand-in nodes with `assert.ok(a === b, '…')` rather than `assert.equal`.

After a change of node version, run `go test -run TestNodeSafeAssert ./web/`
first: the preload rests on a behaviour of node's assert that a new version
could change.
