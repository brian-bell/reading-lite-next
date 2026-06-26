// Package main provides the reader operator CLI entrypoint.
package main

import (
	"os"

	"github.com/bbell/reading-lite/internal/readerctl"
)

func main() {
	os.Exit(readerctl.Main(os.Args[1:], os.Stdout, os.Stderr))
}
