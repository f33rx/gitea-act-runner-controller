# End-to-end proof (kind + Gitea)

`run.sh` stands up a kind cluster and Gitea, deploys the operator, triggers a workflow,
and asserts the full ephemeral-runner lifecycle: runners scale up, the job succeeds, and
everything tears down to zero (pods, EphemeralRunner CRs, and Gitea runner rows) with no
reconcile errors. It is the CI form of the manual dev flow in `../gitea/README.md`.

## Run it

```
make e2e         # create cluster, run the proof, delete the cluster
make e2e-keep    # same, but leave the cluster up for inspection
```

Or directly:

```
./dev/e2e/run.sh
```

Env knobs: `CLUSTER` (default `garc-dev`), `KEEP=1` (don't delete the cluster on exit),
`TIMEOUT` (per-phase wait budget, seconds), `SKIP_IMAGE_BUILD=1` (use an image the caller
already built + loaded, instead of chaining the Makefile's mise/zsh docker build -- this
is what CI sets).

## CI

`.github/workflows/e2e.yml` runs this on pull requests and pushes to `main`: it builds the
controller image with plain docker, loads it into a kind cluster (`helm/kind-action`), and
runs the proof with `SKIP_IMAGE_BUILD=1`. On failure it dumps controller/listener logs and
runner state.

## Validating changes locally

Validate changes to this harness (or the operator behavior it exercises) by running the
**script directly** against a local kind cluster -- `make e2e`, or `make e2e-keep` and then
inspect. This reproduces CI failures faithfully because the moving parts (kind, Gitea seed,
operator deploy, assertions) are the script's, not the GitHub Actions layer's.

`nektos/act` is deliberately **not** used for this workflow: the e2e job creates a kind
cluster inside the job, so running it under `act` would mean kind-inside-a-container
(nested Docker), which needs a privileged DinD runner image and is brittle. Running the
script directly is simpler and closer to what actually breaks. (`act` could still run the
fast `ci.yml` unit gates locally if desired, but those are cheap to check other ways.)
