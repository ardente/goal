package pipe

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync/atomic"
)

var ErrOvercap = errors.New("Buffer overcap")

func notify(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

type ringbuf struct {
	pbits *uint64 // highest bit - close flag. next 31 bits: read pos, next bit - unused, next 31 bits: read avail
	mem   []byte
	mask  int
	wsig  chan struct{}
	rsig  chan struct{}

	synchronized int
	lsig         chan struct{}
	lck          int32
	lq           int32
}

const low63bits = ^uint64(0) >> 1
const low31bits = ^uint32(0) >> 1

const closeFlag = low63bits + 1

const headerFlagMask = closeFlag

const defaultBufferSize = 32 * 1024
const minBufferSize = 8

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func bitlen(x uint) (n uint) {
	if x >= 0x80000000 {
		x >>= 32
		n += 32
	}
	if x >= 0x8000 {
		x >>= 16
		n += 16
	}
	if x >= 0x80 {
		x >>= 8
		n += 8
	}
	if x >= 0x8 {
		x >>= 4
		n += 4
	}

	if x >= 0x2 {
		x >>= 2
		n += 2
	}
	if x >= 0x1 {
		n++
	}
	return n
}

func (b *ringbuf) init(max int, synchronized bool) {
	if max == 0 {
		max = defaultBufferSize
	} else if max < minBufferSize {
		max = minBufferSize
	} else if (max & (max - 1)) != 0 {
		// round up to power of two
		max = 1 << bitlen(uint(max))
	}
	b.initWith(make([]byte, max), synchronized)
}

func (b *ringbuf) initWith(mem []byte, synchronized bool) {
	b.mem = mem
	b.mask = len(mem) - 1
	b.pbits = new(uint64)
	b.wsig = make(chan struct{}, 1)
	b.rsig = make(chan struct{}, 1)

	if synchronized {
		b.synchronized = 1
		b.lsig = make(chan struct{}, 1)
	}
}

func (b *ringbuf) initFrom(src *ringbuf, sync bool) {
	b.pbits = src.pbits
	b.mem = src.mem
	b.mask = src.mask
	b.wsig = src.wsig
	b.rsig = src.rsig
	if sync {
		b.synchronized = 1
		b.lsig = make(chan struct{}, 1)
	}
}

func (b *ringbuf) loadHeader() (hs uint64, closed bool, readPos int, readAvail int) {
	hs = atomic.LoadUint64(b.pbits)
	closed = (hs & closeFlag) != 0
	readPos = int((hs >> 32) & uint64(low31bits))
	readAvail = int(hs & uint64(low31bits))
	return
}

func (b *ringbuf) Close() error {
	for {
		hs := atomic.LoadUint64(b.pbits)
		if ((hs & closeFlag) != 0) || atomic.CompareAndSwapUint64(b.pbits, hs, hs|closeFlag) {
			if (hs & closeFlag) == 0 {
				close(b.rsig)
				close(b.wsig)
				if b.synchronized != 0 {
					close(b.lsig)
				}
			}
			return nil
		}
		runtime.Gosched()
	}
}

/*
func (b *ringbuf) Reopen() {
	b.rsig = make(chan struct{}, 1)
	b.wsig = make(chan struct{}, 1)
	b.lsig = make(chan struct{}, 1)
	*b.pbits = 0 // reset head and size
	b.lck = 0
	b.lq = 0
}*/

func (b *ringbuf) IsClosed() bool {
	return (atomic.LoadUint64(b.pbits) & closeFlag) != 0
}

// Cap returns capacity of the buffer
func (b *ringbuf) Cap() int {
	return len(b.mem)
}

func (b *ringbuf) unlock() {
	atomic.StoreInt32(&b.lck, 0)
	if atomic.LoadInt32(&b.lq) != 0 && !b.IsClosed() {
		notify(b.lsig)
	}
}

func (b *ringbuf) lock() error {
	// fast path
	lck := atomic.LoadInt32(&b.lck)
	if (lck == 0) && atomic.CompareAndSwapInt32(&b.lck, 0, 1) {
		return nil
	}
	// slow path
	atomic.AddInt32(&b.lq, 1)
	for {
		// first spin some
		for i := 0; i < 100; i++ {
			lck = atomic.LoadInt32(&b.lck)
			if (lck == 0) && atomic.CompareAndSwapInt32(&b.lck, 0, 1) {
				atomic.AddInt32(&b.lq, -1)
				return nil
			}
			runtime.Gosched()
		}
		// then wait notification
		<-b.lsig
		if b.IsClosed() {
			atomic.AddInt32(&b.lq, -1)
			return io.EOF
		}
	}
}

func (b *ringbuf) lockWithContext(ctx context.Context) error {
	// fast path
	lck := atomic.LoadInt32(&b.lck)
	if (lck == 0) && atomic.CompareAndSwapInt32(&b.lck, 0, 1) {
		return nil
	}
	// slow path
	atomic.AddInt32(&b.lq, 1)
	for {
		// first spin some
		for i := 0; i < 100; i++ {
			lck = atomic.LoadInt32(&b.lck)
			if (lck == 0) && atomic.CompareAndSwapInt32(&b.lck, 0, 1) {
				atomic.AddInt32(&b.lq, -1)
				return nil
			}
			runtime.Gosched()
		}
		// then wait notification
		select {
		case <-b.lsig:
		case <-ctx.Done():
			atomic.AddInt32(&b.lq, -1)
			return ctx.Err()
		}
		if b.IsClosed() {
			atomic.AddInt32(&b.lq, -1)
			return io.EOF
		}
	}
}