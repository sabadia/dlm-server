//go:build windows

package platform

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func freeSpace(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, fmt.Errorf("platform: free space %q: %w", dir, err)
	}
	var avail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &totalFree); err != nil {
		return 0, fmt.Errorf("platform: free space %q: %w", dir, err)
	}
	return avail, nil
}
