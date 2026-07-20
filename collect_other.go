//go:build !linux && !windows

package main

// Other platforms still heartbeat (liveness is the core signal) and can report
// anything via [custom] commands; built-in collectors are Linux and Windows.
func collect() map[string]any { return map[string]any{} }

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
