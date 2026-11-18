//go:build !darwin

package sysstat

func cpuTicks() (busy, total uint64)   { return 0, 0 }
func temperatures() (cpu, max float64) { return 0, 0 }
