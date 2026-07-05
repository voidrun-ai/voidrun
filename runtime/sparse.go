package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// SparseCopy preserves sparse holes via cp --sparse=always --reflink=auto -p -f.
func SparseCopy(src, dst string) error {
	cmd := exec.Command("cp", "--sparse=always", "--reflink=auto", "-p", "-f", "--", src, dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sparse copy %s -> %s failed: %w: %s", src, dst, err, string(out))
	}
	return nil
}

// SparseMove renames when src/dst share a filesystem and falls back to
// SparseCopy + unlink on EXDEV (the usual case: shm/tmpfs -> instance FS).
func SparseMove(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return fmt.Errorf("rename %s -> %s: %w", src, dst, err)
	}
	if err := SparseCopy(src, dst); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("sparse move unlink %s: %w", src, err)
	}
	return nil
}
