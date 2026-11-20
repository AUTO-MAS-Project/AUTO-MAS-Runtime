//go:build !windows

package telemetry

// StartupSampleInvitation 仅在 Windows 桌面接入。
func StartupSampleInvitation() string      { return "" }
func readStartupResources() map[string]any { return nil }
