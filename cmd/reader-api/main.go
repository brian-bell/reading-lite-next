// Package main provides the reader API process entrypoint.
package main

import (
	"os"

	"github.com/bbell/reading-lite/internal/readerapi"
)

func main() {
	os.Exit(readerapi.Main(os.Args[1:], os.Environ(), os.Stdout, os.Stderr))
}
