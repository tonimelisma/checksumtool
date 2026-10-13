//go:build !linux

package main

import "os"

func adviseSequential(*os.File) {}

func adviseDone(*os.File) {}
