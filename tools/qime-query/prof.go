package main

import (
	"os"
	"runtime/pprof"
)

// startProfile writes a CPU profile when -cpuprofile is given. Ranking work
// happens on the keystroke path, so being able to point a profiler at it
// without installing an input method is worth the dozen lines.
func startProfile(path string) func() {
	if path == "" {
		return func() {}
	}
	f, err := os.Create(path)
	if err != nil {
		return func() {}
	}
	_ = pprof.StartCPUProfile(f)
	return func() {
		pprof.StopCPUProfile()
		_ = f.Close()
	}
}
