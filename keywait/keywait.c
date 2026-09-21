// harness-keywait: block while a key is physically held, for the harness
// hold-to-peek. Terminals never report key releases, so the preview asks
// macOS for the key state instead.
//
//   harness-keywait [keycode]   (default 49 = space)
//   prints "down" as soon as it sees the key held, then
//   exit 0: the key has just been released
//   exit 2: the key is not down (a tap already over, or macOS does not
//           expose the state); the caller falls back to key repeat timing
#include <ApplicationServices/ApplicationServices.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

static int down(CGKeyCode kc) {
	return CGEventSourceKeyState(kCGEventSourceStateCombinedSessionState, kc);
}

int main(int argc, char **argv) {
	CGKeyCode kc = argc > 1 ? (CGKeyCode)atoi(argv[1]) : 49;
	int seen = 0;
	for (int i = 0; i < 10 && !seen; i++) { // ~150 ms to see it down
		if (down(kc)) seen = 1; else usleep(15000);
	}
	if (!seen) return 2;
	puts("down");
	fflush(stdout);
	while (down(kc)) usleep(10000);
	return 0;
}
