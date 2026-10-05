//go:build !windows

package connect

import (
	"errors"
	"fmt"
	"os"
)

// linkOwnerAuth makes link a symlink to the owner's auth.json, or checks
// that it already is one. A link elsewhere, or a separate file, fails the
// proof.
func linkOwnerAuth(ownerAuth, link string) error {
	target, err := os.Readlink(link)
	switch {
	case err == nil && target == ownerAuth:
	case err == nil:
		return &proofError{fmt.Sprintf("the session's auth.json links to %s, not the owner's login", target)}
	case errors.Is(err, os.ErrNotExist):
		if err := os.Symlink(ownerAuth, link); err != nil {
			return err
		}
	default:
		// Not a symlink: something wrote a separate login into the home.
		return &proofError{"the session's auth.json is not a link to the owner's login"}
	}
	return nil
}
