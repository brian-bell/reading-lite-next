// Package acceptance hosts the blackbox verification harness that automates the
// project's acceptance plan (docs/ACCEPTANCE.md).
//
// The harness lives in build-tagged external test files and runs only under the
// "verify" build tag:
//
//	go test -tags verify ./internal/acceptance/...   # or: make verify
//
// This file exists so the package compiles under the default (untagged) build;
// without it, `go build ./...` would report "build constraints exclude all Go
// files" for this directory.
package acceptance
