//go:build !unix

package audit

import "os"

// lockFile is a no-op where flock is unavailable: run one k2stui per audit
// file on those platforms.
func lockFile(*os.File) (unlock func(), err error) { return func() {}, nil }
