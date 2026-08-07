//go:build darwin && cgo

package tunscope

/*
#cgo LDFLAGS: -framework IOKit
#include <stdint.h>

// IOPMConnectionGetSystemCapabilities is exported by IOKit on supported
// macOS releases. Its public source defines bit 0 as CPU availability and bit
// 1 as display/video availability. A dark wake has CPU without video; a full
// user wake has both.
extern uint32_t IOPMConnectionGetSystemCapabilities(void) __attribute__((weak_import));

static int tunscope_system_is_full_wake(void) {
	const uint32_t capabilityCPU = 0x1;
	const uint32_t capabilityVideo = 0x2;
	// Preserve the original fail-safe behavior if a future macOS release no
	// longer exports the capability query.
	if (IOPMConnectionGetSystemCapabilities == NULL) {
		return 1;
	}
	uint32_t capabilities = IOPMConnectionGetSystemCapabilities();
	return (capabilities & capabilityCPU) != 0 &&
	       (capabilities & capabilityVideo) != 0;
}
*/
import "C"

func systemIsFullWake() bool {
	return C.tunscope_system_is_full_wake() != 0
}
