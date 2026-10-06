//go:build unix

package audit

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive advisory lock so two k2stui processes sharing
// one audit file cannot interleave entries or fork the chain.
func lockFile(f *os.File) (unlock func(), err error) {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return nil, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }, nil
}
