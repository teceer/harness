//go:build darwin

package sysstat

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <mach/mach.h>
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitLib.h>

// Private IOHIDEventSystem API: the only way to read Apple Silicon
// temperature sensors without root (what macmon and Stats use).
typedef struct __IOHIDEventSystemClient *IOHIDEventSystemClientRef;
typedef struct __IOHIDServiceClient *IOHIDServiceClientRef;
typedef struct __IOHIDEvent *IOHIDEventRef;
IOHIDEventSystemClientRef IOHIDEventSystemClientCreate(CFAllocatorRef);
int IOHIDEventSystemClientSetMatching(IOHIDEventSystemClientRef, CFDictionaryRef);
CFArrayRef IOHIDEventSystemClientCopyServices(IOHIDEventSystemClientRef);
IOHIDEventRef IOHIDServiceClientCopyEvent(IOHIDServiceClientRef, int64_t, int32_t, int64_t);
CFTypeRef IOHIDServiceClientCopyProperty(IOHIDServiceClientRef, CFStringRef);
double IOHIDEventGetFloatValue(IOHIDEventRef, int32_t);

#define kTempEvent 15

static int cpu_ticks(unsigned long long *busy, unsigned long long *total) {
	host_cpu_load_info_data_t info;
	mach_msg_type_number_t n = HOST_CPU_LOAD_INFO_COUNT;
	if (host_statistics(mach_host_self(), HOST_CPU_LOAD_INFO, (host_info_t)&info, &n) != KERN_SUCCESS) return -1;
	unsigned long long u = info.cpu_ticks[CPU_STATE_USER], s = info.cpu_ticks[CPU_STATE_SYSTEM],
		i = info.cpu_ticks[CPU_STATE_IDLE], ni = info.cpu_ticks[CPU_STATE_NICE];
	*busy = u + s + ni; *total = u + s + ni + i;
	return 0;
}

// temps fills mean of the CPU die sensors ("tdie") and the hottest sensor.
static void temps(double *cpu, double *max) {
	*cpu = 0; *max = 0;
	IOHIDEventSystemClientRef sys = IOHIDEventSystemClientCreate(kCFAllocatorDefault);
	if (!sys) return;
	int page = 0xff00, usage = 5;
	CFNumberRef p = CFNumberCreate(NULL, kCFNumberIntType, &page), u = CFNumberCreate(NULL, kCFNumberIntType, &usage);
	const void *keys[] = {CFSTR("PrimaryUsagePage"), CFSTR("PrimaryUsage")}, *vals[] = {p, u};
	CFDictionaryRef match = CFDictionaryCreate(NULL, keys, vals, 2, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	IOHIDEventSystemClientSetMatching(sys, match);
	CFArrayRef svcs = IOHIDEventSystemClientCopyServices(sys);
	double sum = 0; int n = 0;
	if (svcs) {
		for (CFIndex k = 0; k < CFArrayGetCount(svcs); k++) {
			IOHIDServiceClientRef sc = (IOHIDServiceClientRef)CFArrayGetValueAtIndex(svcs, k);
			IOHIDEventRef ev = IOHIDServiceClientCopyEvent(sc, kTempEvent, 0, 0);
			if (!ev) continue;
			double t = IOHIDEventGetFloatValue(ev, kTempEvent << 16);
			CFRelease(ev);
			if (t <= 0 || t > 150) continue;
			if (t > *max) *max = t;
			CFStringRef name = (CFStringRef)IOHIDServiceClientCopyProperty(sc, CFSTR("Product"));
			if (name) {
				if (CFStringFind(name, CFSTR("tdie"), 0).location != kCFNotFound) { sum += t; n++; }
				CFRelease(name);
			}
		}
		CFRelease(svcs);
	}
	if (n) *cpu = sum / n;
	CFRelease(match); CFRelease(p); CFRelease(u); CFRelease(sys);
}
*/
import "C"

func cpuTicks() (busy, total uint64) {
	var b, t C.ulonglong
	if C.cpu_ticks(&b, &t) != 0 {
		return 0, 0
	}
	return uint64(b), uint64(t)
}

func temperatures() (cpu, max float64) {
	var c, m C.double
	C.temps(&c, &m)
	return float64(c), float64(m)
}
