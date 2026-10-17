package domain

// Segment is a byte range of a file downloaded by one connection.
type Segment struct {
	Index int `json:"index"`
	// Start is the first byte offset (inclusive).
	Start int64 `json:"start"`
	// End is the end offset (exclusive).
	End int64 `json:"end"`
	// Done is the number of bytes completed from Start.
	Done int64 `json:"done"`
}

// Len returns the segment length in bytes.
func (s Segment) Len() int64 { return s.End - s.Start }

// Remaining returns the bytes still to download.
func (s Segment) Remaining() int64 { return s.Len() - s.Done }

// Complete reports whether every byte of the segment is downloaded.
func (s Segment) Complete() bool { return s.Done >= s.Len() }

// Validate checks 0 <= Start < End and 0 <= Done <= Len. It returns an
// error matching ErrInvalidArgument.
func (s Segment) Validate() error {
	if s.Start < 0 || s.Start >= s.End {
		return InvalidArgument("segment %d: invalid range [%d, %d)", s.Index, s.Start, s.End)
	}
	if s.Done < 0 || s.Done > s.Len() {
		return InvalidArgument("segment %d: done %d out of range 0..%d", s.Index, s.Done, s.Len())
	}
	return nil
}
