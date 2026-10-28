package daemon

import (
	"os"
)

func pid() int { return os.Getpid() }

// databaseSize returns the size of the SQLite database including its
// write-ahead log; a missing file counts as 0.
func databaseSize(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	return total
}
