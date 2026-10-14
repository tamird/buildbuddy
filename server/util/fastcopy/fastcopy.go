package fastcopy

import "flag"

var enableFastcopyReflinking = flag.Bool("executor.enable_fastcopy_reflinking", false, "If true, attempt to use `cp --reflink=auto` to link files")

// ReflinkingEnabled returns whether Clone makes copy-on-write copies instead of
// hard links.
func ReflinkingEnabled() bool {
	return *enableFastcopyReflinking
}
