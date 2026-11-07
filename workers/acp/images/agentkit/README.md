# AgentKit composition

The final stage inherits the frozen AgentKit image without executing any `RUN`
in it. A native `supervisor-builder` derivative mounts `runtime-source`
read-only, checks the matching lowercase SHA-256 image/adapter digests and source
contents, and prepares a filesystem overlay. The final stage copies that overlay
before the existing static binaries and license documents; its runtime Config
instructions are unchanged. No build shell or helper is copied into the runtime.

Only `/opt/agentkit`, `/agent`, `/sessions`, and `/usr/share/licenses/orka` are
copied with their complete existing contents. Existing parent directory metadata
(`/`, `/opt`, `/usr`, `/usr/share`, `/usr/share/licenses`) is carried separately
without recursively copying their unrelated children. Ownership and permissions
are normalized exactly as the previous final-stage shell commands did for
ordinary directories and files.

Executable/nonempty checks resolve symlink components in the mounted source's
namespace. For example, an absolute `/usr/local/bin/python` link is checked in
the source image, not against the builder's interpreter. Checking executability
has the original `test -x` semantics: it does not execute the file, parse a
shebang, or prove that an interpreter/shared library can run. Internal venv
symlinks remain symlinks when copied; their targets outside the overlay remain
in the inherited source image.

Symlinked overlay roots or parent directories are rejected. An `agent.yaml`
symlink may point within `/agent`, but an external target is rejected because
normalizing its metadata would require changing a fifth tree. These restrictions
are narrower than the old shell's behavior for unusual source-image layouts.
Directory owner/mode preservation through the final BuildKit `COPY` and full
runtime Config equivalence still require an old/new image comparison; fixture
unit tests exercise preparation, not BuildKit's merge implementation.

Run the tests with root privileges inside a disposable Debian-based container
with Python and GNU coreutils/tar (the same tools used by the Go builder):

```sh
python3 -B -m unittest discover -s workers/acp/images/agentkit -v
```

Without root, ownership tests skip. Tests cover stage isolation, targeted copy,
source immutability, inherited parent metadata, absolute executable symlinks,
nonexecutable source targets, empty configuration, digest rejection, and
symlinked-directory rejection, creation of missing optional directories, and
normalization through an internal configuration symlink.
