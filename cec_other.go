//go:build !linux

package main

// The CEC adapter only exists on the device. On the build host the session
// still compiles and its logic is unit tested with a fake adapter; opening
// the real thing reports that it is unavailable.

import "fmt"

func openCEC(path string, log *Logger) (cecAdapter, error) {
	return nil, fmt.Errorf("CEC is only available on Linux (%s)", path)
}

func cecDeviceExists(path string) bool { return false }
