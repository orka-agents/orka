# Answer formats

Use these only when the caller asks for them, for example for an eval, a tracker or
automation.

## Structured answer

```
**Answer:** <Yes. | Only on main. | No. | Can't tell from here. | Here's how.> <one or two sentences>
**Status:** released in vX.Y.Z | on main since <PR>, not in vX.Y.Z | proposed in <issue/PR> | not supported | n/a (concept)
**How:** <minimal steps, commands or YAML>
**Evidence:** `path:line` (at <version>): what it shows, one per line
**Caveats:** <limits, drift, anything you couldn't verify>
```

## Gap lines

Add one line per gap after the answer:

```
gap: category=<unsupported_request|doc_gap|drift|unanswerable> topic=<short-kebab> evidence=<path:line or issue URL> tracking=<issue/PR URL or none>
```

`unsupported_request`: Orka lacks it. `doc_gap`: it works but no doc explains it. `drift`:
docs, code, chart, release or tracker disagree. `unanswerable`: the sources can't settle it.
