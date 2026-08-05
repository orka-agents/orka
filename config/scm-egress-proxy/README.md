# SCM egress proxy

This package is included by `config/default`. Before applying it, create the
shared Publisher/proxy authentication Secret in `orka-system`:

```bash
token="$(openssl rand -hex 32)"
kubectl -n orka-system create secret generic scm-egress-proxy-auth \
  --from-literal=token="$token"
unset token
```

The token must contain 32-256 RFC 3986 unreserved characters (`A-Z`, `a-z`,
`0-9`, `-`, `.`, `_`, or `~`). It authenticates the Publisher to the proxy; it
is not an SCM credential.

`deployment.yaml` allows only `github.com` plus `api.github.com`. Patch
`--allowed-hosts` and `--forge-api-base-url` together with the Publisher's
`ORKA_PUBLISHER_ALLOWED_SCM_HOSTS` and forge API URL when using GitHub
Enterprise or another reviewed forge endpoint. Hostnames are exact and
lower-case; wildcards and IP literals are rejected.
