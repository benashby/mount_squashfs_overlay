//go:build !windows

package main

import (
	"fmt"
	"os"
)

func shellMount([]string) int {
	fmt.Fprintln(os.Stderr, "shell-mount is only available on Windows")
	return 1
}
