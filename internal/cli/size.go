package cli

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// sizeUnits maps upper-cased size suffixes to their multiplier. Single
// letters (K, M, G, T) are binary like curl and aria2; SI suffixes (KB, MB,
// ...) are decimal; IEC suffixes (KiB, MiB, ...) are binary.
var sizeUnits = map[string]float64{
	"":    1,
	"B":   1,
	"K":   1 << 10,
	"KB":  1e3,
	"KIB": 1 << 10,
	"M":   1 << 20,
	"MB":  1e6,
	"MIB": 1 << 20,
	"G":   1 << 30,
	"GB":  1e9,
	"GIB": 1 << 30,
	"T":   1 << 40,
	"TB":  1e12,
	"TIB": 1 << 40,
}

// sizeHelp documents the accepted size syntax for flag help.
const sizeHelp = "bytes, or a number with a unit: K/M/G (×1024), KB/MB/GB (×1000), KiB/MiB/GiB (×1024); 0 = unlimited"

// parseSize parses a human-friendly byte size such as "512K", "1.5MB",
// "2MiB" or "0" (see sizeUnits). Fractions are rounded down to whole bytes.
func parseSize(s string) (int64, error) {
	in := strings.TrimSpace(s)
	i := strings.IndexFunc(in, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	num, unit := in, ""
	if i >= 0 {
		num, unit = in[:i], strings.TrimSpace(in[i:])
	}
	mult, ok := sizeUnits[strings.ToUpper(unit)]
	if num == "" || !ok {
		return 0, fmt.Errorf("cli: invalid size %q (%s)", s, sizeHelp)
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("cli: invalid size %q: %w", s, errors.Unwrap(err))
	}
	bytes := math.Floor(v * mult)
	if bytes >= math.MaxInt64 {
		return 0, fmt.Errorf("cli: size %q is too large", s)
	}
	return int64(bytes), nil
}

// formatSize renders a byte count with binary units ("1.5 MiB"); negative
// sizes (unknown) render as "-".
func formatSize(n int64) string {
	if n < 0 {
		return "-"
	}
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
