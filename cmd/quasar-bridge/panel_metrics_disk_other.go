//go:build !linux && !darwin

package main

import "errors"

// stationDiskUsage is only meaningful on the station itself. On a development
// machine the metric stays null, which the panel draws as a gap instead of as a
// wrong number.
func stationDiskUsage(string) (usedMB, totalMB float64, err error) {
	return 0, 0, errors.New("disk usage is not available on this platform")
}
