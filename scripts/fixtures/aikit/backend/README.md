# Qwen recurrent-prefix checkpoint correction

This build keeps the prebuilt AIKit Qwen 3.5 4B image and model weights, replacing
only its CPU llama.cpp backend. It uses the same source revisions as the base:

- LocalAI `7ad0cbf259f0c7bf9920fe2438fc3630ecd6c672`
- llama.cpp `38a5b42d9a3e82e0a586bcd1caed121f36c87a73`

LocalAI's completion adapters omit the template's message-boundary metadata.
llama.cpp also normally saves recurrent checkpoints only at user boundaries or
near the prompt end. That does not cover Orka's changing cluster context inside
long system prompts. The correction restores boundary metadata for both
completion paths and permits bounded mid-prompt checkpoints for completion
requests. Model eligibility, checkpoint count/spacing, multimodal exclusions,
score behavior, and request limits remain intact.

Run the build on the VM, not locally. Prepare an empty build directory there,
extract the pinned LocalAI source, and copy this Dockerfile plus `patch.py`:

```sh
curl -fsSL https://codeload.github.com/mudler/LocalAI/tar.gz/7ad0cbf259f0c7bf9920fe2438fc3630ecd6c672 \
  | tar -xz --strip-components=1 -C "$build_dir"
cp patch.py "$build_dir/orka-checkpoint-patch.py"
cp Dockerfile "$build_dir/orka-checkpoint.Dockerfile"
docker buildx build --platform linux/amd64 \
  -f "$build_dir/orka-checkpoint.Dockerfile" \
  -t "$task_image" --push "$build_dir"
```

CI uses the resulting prebuilt image by digest. It does not build this backend
or download model weights separately. Qualification still runs on the free
GitHub-hosted runner with the existing model resource and live-request limits.

The source-transform contracts run without Docker or a model:

```sh
python3 scripts/fixtures/aikit/backend/patch_test.py
```
