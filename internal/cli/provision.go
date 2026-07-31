package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bitomule/simpool/internal/pool"
)

// acquireAndProvision locks `count` slots and makes sure each has a booted
// simulator ready to use. On any failure, every slot acquired so far is
// released before returning — callers never have to clean up a partial
// acquisition themselves.
func acquireAndProvision(device, osVersion string, count int, ownerCmd string) (slots []*pool.Slot, runDir string, err error) {
	root, err := pool.Root()
	if err != nil {
		return nil, "", err
	}

	slots, err = pool.AcquireSlots(root, device, osVersion, count)
	if err != nil {
		return nil, "", err
	}

	release := func() {
		for _, s := range slots {
			s.Release()
		}
	}

	for _, s := range slots {
		if err := pool.EnsureProvisioned(s, ownerCmd); err != nil {
			release()
			return nil, "", fmt.Errorf("slot %s: %w", s.Dir, err)
		}
	}

	runDir = filepath.Join(slots[0].Dir, "runs", fmt.Sprintf("%d-%s", os.Getpid(), nowStamp()))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		release()
		return nil, "", err
	}

	return slots, runDir, nil
}

func releaseAll(slots []*pool.Slot) {
	for _, s := range slots {
		s.Release()
	}
}
