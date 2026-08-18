//go:build darwin && cgo

package tunscope

/*
#cgo LDFLAGS: -framework CoreFoundation -framework SystemConfiguration
#include <CoreFoundation/CoreFoundation.h>
#include <SystemConfiguration/SystemConfiguration.h>
#include <dispatch/dispatch.h>
#include <stdint.h>
#include <stdlib.h>
#include <unistd.h>

typedef struct {
	SCDynamicStoreRef store;
	dispatch_queue_t queue;
	int write_fd;
} tunscope_network_watcher;

static void tunscope_network_changed(
	SCDynamicStoreRef store,
	CFArrayRef changed_keys,
	void *info
) {
	(void)store;
	(void)changed_keys;
	tunscope_network_watcher *watcher = (tunscope_network_watcher *)info;
	if (watcher == NULL || watcher->write_fd < 0) {
		return;
	}
	uint8_t notification = 1;
	// The pipe is non-blocking. One pending byte is sufficient because Go
	// coalesces a notification burst into a fresh stability window.
	(void)write(watcher->write_fd, &notification, sizeof(notification));
}

static void tunscope_append_interface_key(
	CFMutableArrayRef keys,
	CFStringRef interface_name,
	CFStringRef component
) {
	CFStringRef key = CFStringCreateWithFormat(
		kCFAllocatorDefault,
		NULL,
		CFSTR("State:/Network/Interface/%@/%@"),
		interface_name,
		component
	);
	if (key != NULL) {
		CFArrayAppendValue(keys, key);
		CFRelease(key);
	}
}

static tunscope_network_watcher *tunscope_network_watcher_start(
	const char *interface_name,
	int write_fd,
	int *error_code
) {
	if (error_code != NULL) {
		*error_code = 0;
	}
	tunscope_network_watcher *watcher = calloc(1, sizeof(*watcher));
	if (watcher == NULL) {
		if (error_code != NULL) {
			*error_code = -1;
		}
		return NULL;
	}
	watcher->write_fd = write_fd;

	SCDynamicStoreContext context = {0, watcher, NULL, NULL, NULL};
	watcher->store = SCDynamicStoreCreate(
		kCFAllocatorDefault,
		CFSTR("TunScope physical network monitor"),
		tunscope_network_changed,
		&context
	);
	if (watcher->store == NULL) {
		if (error_code != NULL) {
			*error_code = SCError();
		}
		free(watcher);
		return NULL;
	}

	CFStringRef interface_string = CFStringCreateWithCString(
		kCFAllocatorDefault,
		interface_name,
		kCFStringEncodingUTF8
	);
	CFMutableArrayRef keys = CFArrayCreateMutable(
		kCFAllocatorDefault,
		0,
		&kCFTypeArrayCallBacks
	);
	if (interface_string == NULL || keys == NULL) {
		if (error_code != NULL) {
			*error_code = -1;
		}
		if (interface_string != NULL) {
			CFRelease(interface_string);
		}
		if (keys != NULL) {
			CFRelease(keys);
		}
		CFRelease(watcher->store);
		free(watcher);
		return NULL;
	}

	// Link is republished for Wi-Fi BSSID changes even when the interface,
	// address, and gateway stay identical. IPv4 and DHCP cover same-address
	// lease renewal and route-epoch replacement on other physical links.
	tunscope_append_interface_key(keys, interface_string, CFSTR("Link"));
	tunscope_append_interface_key(keys, interface_string, CFSTR("IPv4"));
	CFArrayAppendValue(keys, CFSTR("State:/Network/Global/IPv4"));

	CFPropertyListRef global_value = SCDynamicStoreCopyValue(
		watcher->store,
		CFSTR("State:/Network/Global/IPv4")
	);
	if (global_value != NULL &&
		CFGetTypeID(global_value) == CFDictionaryGetTypeID()) {
		CFTypeRef service_value = CFDictionaryGetValue(
			(CFDictionaryRef)global_value,
			CFSTR("PrimaryService")
		);
		if (service_value != NULL &&
			CFGetTypeID(service_value) == CFStringGetTypeID()) {
			CFStringRef dhcp_key = CFStringCreateWithFormat(
				kCFAllocatorDefault,
				NULL,
				CFSTR("State:/Network/Service/%@/DHCP"),
				(CFStringRef)service_value
			);
			if (dhcp_key != NULL) {
				CFArrayAppendValue(keys, dhcp_key);
				CFRelease(dhcp_key);
			}
		}
	}
	if (global_value != NULL) {
		CFRelease(global_value);
	}
	CFRelease(interface_string);

	if (!SCDynamicStoreSetNotificationKeys(watcher->store, keys, NULL)) {
		if (error_code != NULL) {
			*error_code = SCError();
		}
		CFRelease(keys);
		CFRelease(watcher->store);
		free(watcher);
		return NULL;
	}
	CFRelease(keys);

	watcher->queue = dispatch_queue_create(
		"com.gfheng.tunscope.physical-network-events",
		DISPATCH_QUEUE_SERIAL
	);
	if (watcher->queue == NULL) {
		if (error_code != NULL) {
			*error_code = -1;
		}
		CFRelease(watcher->store);
		free(watcher);
		return NULL;
	}
	if (!SCDynamicStoreSetDispatchQueue(watcher->store, watcher->queue)) {
		if (error_code != NULL) {
			*error_code = SCError();
		}
#if !OS_OBJECT_USE_OBJC
		dispatch_release(watcher->queue);
#endif
		CFRelease(watcher->store);
		free(watcher);
		return NULL;
	}
	return watcher;
}

static void tunscope_dispatch_barrier(void *context) {
	(void)context;
}

static void tunscope_network_watcher_stop(tunscope_network_watcher *watcher) {
	if (watcher == NULL) {
		return;
	}
	(void)SCDynamicStoreSetDispatchQueue(watcher->store, NULL);
	// Drain a callback that was already queued before its context is freed or
	// the Go side closes the pipe descriptor.
	dispatch_sync_f(watcher->queue, NULL, tunscope_dispatch_barrier);
	watcher->write_fd = -1;
	CFRelease(watcher->store);
#if !OS_OBJECT_USE_OBJC
	dispatch_release(watcher->queue);
#endif
	free(watcher);
}
*/
import "C"

import (
	"fmt"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

type scPhysicalNetworkEventWatcher struct {
	read   *os.File
	write  *os.File
	native *C.tunscope_network_watcher
	events chan physicalNetworkEvent
	once   sync.Once
	readWG sync.WaitGroup
}

func newPhysicalNetworkEventWatcher(interfaceName string) (physicalNetworkEventWatcher, error) {
	readFile, writeFile, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create physical-network notification pipe: %w", err)
	}
	closePipe := func() {
		_ = readFile.Close()
		_ = writeFile.Close()
	}
	if err := unix.SetNonblock(int(writeFile.Fd()), true); err != nil {
		closePipe()
		return nil, fmt.Errorf("make physical-network notification pipe non-blocking: %w", err)
	}

	cInterface := C.CString(interfaceName)
	defer C.free(unsafe.Pointer(cInterface))
	var errorCode C.int
	native := C.tunscope_network_watcher_start(
		cInterface,
		C.int(writeFile.Fd()),
		&errorCode,
	)
	if native == nil {
		closePipe()
		if errorCode == -1 {
			return nil, fmt.Errorf("allocate macOS physical-network watcher")
		}
		return nil, fmt.Errorf("start macOS physical-network watcher: SystemConfiguration error %d", int(errorCode))
	}

	watcher := &scPhysicalNetworkEventWatcher{
		read: readFile, write: writeFile, native: native,
		events: make(chan physicalNetworkEvent, 1),
	}
	watcher.readWG.Add(1)
	go watcher.readNotifications(interfaceName)
	return watcher, nil
}

func (w *scPhysicalNetworkEventWatcher) readNotifications(interfaceName string) {
	defer w.readWG.Done()
	defer close(w.events)
	buffer := make([]byte, 32)
	for {
		count, err := w.read.Read(buffer)
		if count > 0 {
			select {
			case w.events <- physicalNetworkEvent{Description: fmt.Sprintf(
				"macOS reported a link, IPv4, or DHCP epoch change on %s",
				interfaceName,
			)}:
			default:
			}
		}
		if err != nil {
			return
		}
	}
}

func (w *scPhysicalNetworkEventWatcher) Events() <-chan physicalNetworkEvent {
	if w == nil {
		return nil
	}
	return w.events
}

func (w *scPhysicalNetworkEventWatcher) Close() error {
	if w == nil {
		return nil
	}
	w.once.Do(func() {
		C.tunscope_network_watcher_stop(w.native)
		w.native = nil
		_ = w.write.Close()
		w.readWG.Wait()
		_ = w.read.Close()
	})
	return nil
}
