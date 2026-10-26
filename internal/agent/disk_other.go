//go:build !linux

package agent

func readDisk(string) (float64, uint64) { return 0, 0 }
