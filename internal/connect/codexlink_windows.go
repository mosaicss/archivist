//go:build windows

package connect

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// errCodexCrossDrive is the start failure when the session home and the
// owner's Codex home are on different volumes (no hard link possible).
var errCodexCrossDrive = errors.New("cannot link the Codex login across drives; keep CODEX_HOME on the same drive as your user profile")

// linkOwnerAuth makes link an NTFS hard link to the owner's auth.json
// (Story 78.34): a symlink needs Developer Mode or admin rights, a copy
// would split the single use refresh token. Codex rewrites auth.json in
// place (truncate and write), so both names keep naming one file. Before
// every start and resume the link is checked by file identity
// (os.SameFile); a link that no longer is the owner's file (the owner
// logged out and in again, which replaces the file) is replaced by a fresh
// hard link: the owner's login wins.
func linkOwnerAuth(ownerAuth, link string) error {
	owner, err := os.Stat(ownerAuth)
	if err != nil {
		return err
	}
	cur, err := os.Lstat(link)
	switch {
	case err == nil && cur.Mode().IsRegular() && os.SameFile(owner, cur):
		return nil
	case err == nil:
		// Not the owner's file: link afresh beside it, then replace it.
		fresh := link + ".link"
		_ = os.Remove(fresh)
		if err := hardLink(ownerAuth, fresh); err != nil {
			return err
		}
		if err := os.Rename(fresh, link); err != nil {
			_ = os.Remove(fresh)
			return fmt.Errorf("replace the session's link to the Codex login: %w", err)
		}
		return nil
	case errors.Is(err, os.ErrNotExist):
		return hardLink(ownerAuth, link)
	default:
		return err
	}
}

// hardLink is os.Link with the cross volume failure spelled out.
func hardLink(target, link string) error {
	if err := os.Link(target, link); err != nil {
		if errors.Is(err, windows.ERROR_NOT_SAME_DEVICE) {
			return errCodexCrossDrive
		}
		return fmt.Errorf("link the Codex login: %w", err)
	}
	return nil
}
