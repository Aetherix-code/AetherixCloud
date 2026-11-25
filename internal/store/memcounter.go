package store

import "sync/atomic"

var memSeq atomic.Int64

func memCounter() int64 { return memSeq.Add(1) }
