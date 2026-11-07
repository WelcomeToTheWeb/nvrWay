# Agent Note: detector-image-offline-criterion-buildkit

Status: implemented

## Problem

S9-02's exit criterion "build succeeds offline for stage 2 (stage 1 needs network and says so loudly)" cannot be verified literally: BuildKit includes the RUN network mode in the layer cache key, so `docker build --network=none` re-executes every pip layer — no Dockerfile that pip-installs can ever build with that flag, warm cache or not. A naive reading would call the criterion failed; the real property the criterion protects is that stage 2's rebuild never re-does stage 1's network-heavy work (torch download, ONNX export).

## Decision

The criterion is verified as two demonstrated halves. (1) Stage-2-only rebuild: with the runtime stage named (`FROM python:3.12-slim AS runtime`), a real content change in detector/ + `docker build --target runtime` shows all five export-stage steps CACHED and only stage 2's COPY detector/ + pip install -e . re-running — stage 1's network work is fully bypassed. (2) Stage 1 fails loudly offline: `--network=none` dies at stage 1's first RUN with pip's `No matching distribution found for ultralytics==8.4.195`. The in-image model is proven genuine by hash: sha256 of /models/yolov8n.onnx equals the exported[] entry in testdata/fixtures.json (d23c1262…), and the slow smoke (tests/test_model_smoke.py, -m slow) passes 2/2 inside the image with testdata/ mounted at /testdata. The runtime image carries pytest precisely so the smoke runs there, never on the host.
## Alternatives considered

Literal `--network=none --target runtime` build: impossible — the flag invalidates all RUN caches, so even stage 2's own pip layer re-runs and fails; rejected as unverifiable, replaced by the two-halves reading. `--no-cache-filter export` to force stage 1 cold offline: rejected as redundant — plain --network=none already re-runs stage 1's first pip layer and fails loudly, which is the half being demonstrated. Copying the smoke fixtures into the image instead of mounting testdata/: deferred to S10-01/S10-03 (fetch-fixture.sh owns fixture provenance); mounting keeps the image free of gitignored assets.
## Consequences

Bought: CI (S9-09) and verify.sh (S10-03) can rebuild the detector image on every push cheaply — the export stage is a pure function of the pinned pip layers and the pinned model URL, cached across builds; the hash check makes model provenance a one-liner (`docker run --entrypoint python … hashlib`). Cost: the `--network=none` flag is now known-bad vocabulary for this repo's CI — anyone writing an "offline build" gate must use the two-halves formulation or the gate will fail for the wrong reason. The card's exit-criteria text names tests/test_pipeline_smoke.py; the real file is tests/test_model_smoke.py (recorded in the card).

