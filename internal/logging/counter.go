package logging

import "sync/atomic"

// atomicCounter is a uint64 that can be added to from several goroutines.
type atomicCounter struct{ n atomic.Uint64 }

func (c *atomicCounter) Add(d uint64) { c.n.Add(d) }
func (c *atomicCounter) Load() uint64 { return c.n.Load() }
