//go:build !windows

package main

import "os"

func enableVT(*os.File) bool { return true }
