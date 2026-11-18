/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package repocheck holds tests that enforce repository rules from AGENTS.md
// which no linter covers. Each failure says how to fix the problem.
//
// Run them with: go test -count=1 ./hack/repocheck
//
// -count=1 matters: the scans list files through git, which the Go test
// cache cannot always see, so a cached pass could miss a newly staged file.
package repocheck
