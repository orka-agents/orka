// Modified from open-policy-agent/gatekeeper cmd/build/helmify at
// c9b67657102032a460a28e7f3b9c88ec0c193453.
package main

// replacements is intentionally empty while Orka's non-CRD Helm templates
// remain static inputs. Future manifest-derived templates should introduce
// explicit HELMSUBST sentinels and replacements here.
var replacements = map[string]string{}
