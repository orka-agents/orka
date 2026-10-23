#!/bin/sh
# Runs only in the native Go builder, against a read-only source-image mount.
set -eu

source=$1
overlay=$2
fail() { printf '%s\n' "$*" >&2; exit 1; }

case "$AGENTKIT_RUNTIME_IMAGE" in *@sha256:*) ;; *) fail "AGENTKIT_RUNTIME_IMAGE must be digest-pinned" ;; esac
digest=${AGENTKIT_RUNTIME_IMAGE##*@sha256:}
test "${#digest}" -eq 64 || fail "AGENTKIT_RUNTIME_IMAGE digest must have 64 characters"
case "$digest" in *[!0-9a-f]*) fail "AGENTKIT_RUNTIME_IMAGE must use a lowercase sha256 digest" ;; esac
case "$AGENTKIT_ADAPTER_DIGEST" in sha256:*) ;; *) fail "AGENTKIT_ADAPTER_DIGEST must be a sha256 digest" ;; esac
adapter_digest=${AGENTKIT_ADAPTER_DIGEST#sha256:}
test "${#adapter_digest}" -eq 64 || fail "AGENTKIT_ADAPTER_DIGEST digest must have 64 characters"
case "$adapter_digest" in *[!0-9a-f]*) fail "AGENTKIT_ADAPTER_DIGEST must use a lowercase sha256 digest" ;; esac
test "$AGENTKIT_ADAPTER_DIGEST" = "sha256:$digest" || fail "AGENTKIT_ADAPTER_DIGEST must equal the digest-pinned AgentKit source image"

# Absolute links belong to the source root, not the builder. Do not execute the
# runtime's interpreter (which may also be for a different architecture).
resolve_source() (
    pending=$1
    resolved=
    links=0
    while test -n "$pending"; do
        component=${pending%%/*}
        if test "$pending" = "$component"; then pending=; else pending=${pending#*/}; fi
        case "$component" in
            ''|.) continue ;;
            ..) resolved=${resolved%/*} ;;
            *)
                candidate=$source$resolved/$component
                if test -L "$candidate"; then
                    links=$((links + 1))
                    test "$links" -le 40 || fail "too many source symlinks"
                    target=$(readlink "$candidate")
                    case "$target" in /*) resolved= ;; esac
                    pending=$target${pending:+/$pending}
                else
                    if test -n "$pending"; then test -d "$candidate" || fail "source path component is not a directory: $candidate"; fi
                    resolved=$resolved/$component
                fi
                ;;
        esac
    done
    printf '%s\n' "$source$resolved"
)

# A symlinked overlay directory could make chmod/chown escape the four trees.
# Reject that layout instead of following a link into the builder or widening
# the overlay to unrelated source paths.
for path in /opt /opt/agentkit /agent /sessions /usr /usr/share /usr/share/licenses /usr/share/licenses/orka; do
    test ! -L "$source$path" || fail "overlay directory must not be a symlink: $path"
    if test -e "$source$path"; then test -d "$source$path" || fail "overlay path must be a directory: $path"; fi
done
test -d "$source/opt/agentkit" || fail "source /opt/agentkit directory is missing"
test -d "$source/agent" || fail "source /agent directory is missing"
serve=$(resolve_source /opt/agentkit/bin/agentkit-serve)
test -x "$serve" || fail "agentkit-serve must be executable in the source image"
config=$(resolve_source /agent/agent.yaml)
test -s "$config" || fail "agent.yaml must be nonempty in the source image"
case "$config" in "$source"/agent/*) ;; *) fail "agent.yaml symlink target must stay under /agent" ;; esac

test ! -e "$overlay" || fail "prepared overlay already exists"
mkdir -p "$overlay"
# COPY merges these parent directories too. Archive only their metadata, not
# their children; cp -a below preserves all contents of the four changed trees.
set -- .
for path in /opt /usr /usr/share /usr/share/licenses; do
    if test -d "$source$path"; then set -- "$@" ".$path"; fi
done
tar --no-recursion --xattrs --acls -C "$source" -cpf "$overlay.parents.tar" "$@"
tar --xattrs --acls -C "$overlay" -xpf "$overlay.parents.tar"
rm "$overlay.parents.tar"
mkdir -p "$overlay/opt" "$overlay/usr/share/licenses"
cp -a "$source/opt/agentkit" "$overlay/opt/agentkit"
cp -a "$source/agent" "$overlay/agent"
for path in /sessions /usr/share/licenses/orka; do
    if test -d "$source$path"; then cp -a "$source$path" "$overlay$path"; else mkdir -p "$overlay$path"; fi
done
chown -R 0:0 "$overlay/opt/agentkit"
chmod -R a+rX,go-w "$overlay/opt/agentkit"
chown 0:0 "$overlay/agent" "$overlay${config#"$source"}"
chmod 0555 "$overlay/agent"
chmod 0711 "$overlay/sessions"
chmod 0444 "$overlay${config#"$source"}"
