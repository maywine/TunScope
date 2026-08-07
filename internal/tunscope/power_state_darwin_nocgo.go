//go:build darwin && !cgo

package tunscope

// Non-cgo Darwin builds cannot query IOKit power capabilities. Preserve the
// original fail-safe timeout behavior instead of silently disabling it.
func systemIsFullWake() bool { return true }
