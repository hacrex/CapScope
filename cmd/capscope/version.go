package main

import "fmt"

// runVersion prints the build version.
func runVersion() error {
	fmt.Printf("capscope %s\n", version)
	return nil
}
