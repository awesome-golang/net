package timeout

import (
	"container/heap"
	"sync"
	"sync/atomic"
	"time"
	_ "unsafe" // must import in order to use go:linkname directive below
)

// Timeouts are handled using a Daemon, which runs a single daemon
// goroutine that checks to see when the next timeout will occur,
// sleeps until that time, and then performs whatever action is
// associated with that timeout. In addition to sleeping, the daemon
// also waits on a "wake" channel, which is writetn to whenever
// a new timeout is added or the daemon is stopped. If a timeout
// is added which is sooner than the current soonest timeout, this
// ensures that the daemon wakes up and handles it on-time rather
// than sleeping until the later time. Similarly, it ensures that
// closes are detected immediately rather than only after the next
// scheduled deadline (which could be arbitrarily far in the future,
// leading to a resource leak).
//
// Timeouts may also be cancelled. In these cases, it's desirable to
// avoid having the daemon acquire the global lock on the Conn
// when it's technically not necessary (having found the timeout
// cancelled, the daemon will then just release the lock without
// doing any work). Thus, each timeout object has a cancel field.
// When a timeout is cancelled, the goroutine doing the cancelling
// atomically sets the cancel field to 1. Then, when the daemon
// wakes up from sleep, it atomically loads the cancel field. If the
// field has been set to 1, then the daemon simply throws away the
// timeout object, and waits for the next timeout. If the field is
// still 0, the daemon acquires the global Conn lock (actually, it
// releases the Daemon lock and then acquires the Conn lock and
// then the Daemon lock (in that order) to avoid a deadlock
// with another goroutine, having already acquired the Conn lock,
// calling AddTimeout and thus trying to acquire the Daemon lock).
// However, there's a chance that in between the atomic load of the
// cancel field and acquiring the Conn lock, another goroutine acquired
// the Conn lock, did some work, and canceled the timeout. Thus, after
// acquiring the Conn lock, the daemon must re-check the cancel field.
// If the cancel field is 1, the daemon immediately releases the
// global Conn lock, throws the timeout away, and waits for the next
// timeout.
//
// It is the responsibility of the goroutine doing the cancelling
// to clear any record of the timeout from the Conn object. If a
// timeout has not been cancelled by the time that the daemon
// acquires the global Conn lock, then the timeout's callback is
// executed. In this case, it is the responsibility of the callback
// to clear any record of the timeout from the Conn object.

// A Timeout is a handle on a timeout which allows it to be cancelled.
type Timeout struct {
	f      func()
	t      time.Time
	cancel uint32 // 1 if cancelled, 0 otherwise; only access atomically
}

// Cancel cancels t. The caller must acquire a lock on the locker used
// to construct the related Daemon (in the call to NewDaemon) before
// calling Cancel. Otherwise, the behavior of Cancel is undefined.
func (t *Timeout) Cancel() {
	atomic.StoreUint32(&t.cancel, 1)
}

// A Daemon is a handle on a daemon goroutine which allows for the scheduling
// and execution of timeouts and their related callbacks.
type Daemon struct {
	locker   sync.Locker
	timeouts heapTimeouts
	// used when len(timeouts) == 0 and the daemon needs to
	// wait until there are more timeouts
	cond sync.Cond
	// In case the daemon is sleeping when a new timeout
	// is scheduled for earlier than the daemon will wake
	// up, AddTimeout writes to this channel to wake the
	// daemon up. The channel is buffered at least one,
	// and all writes into the channel are selects with
	// a default case. This means that the result of
	// every send to the channel is that one element
	// is in the channel, and the send will never block.
	// Since every send is guaranteed to result in one
	// element in the channel, every send is guaranteed
	// to cause the daemon to read a value from the
	// channel if it every attempts to at any point in
	// the future. Once it reads an element, it then
	// immediately acquires the lock. Having acquired the
	// lock, it re-checks for the soonest timeout. Thus,
	// the only time that the channel can be emptied is
	// when the daemon will learn of the most up-to-date
	// soonest timeout, and thus it's safe for the channel
	// to be empty.
	wake chan struct{}
	// used to indicate that the Daemon has been stopped;
	// the daemon must always check this after acquiring
	// mu and before doing any work, returning immediately
	// if stopped == true.
	stopped bool
	mu      sync.Mutex
}

// TODO(joshlf): Any way to make NewDaemon return a Daemon instead of a *Daemon?
// It'd be better for locality to embed the Daemon directly in the Conn struct.
// It would probably require something like an Init method instead of using
// NewDaemon.

// NewDaemon starts a new daemon and returns a handle to it.
// A lock on locker will be acquired before any timeout's
// callback is executed.
func NewDaemon(locker sync.Locker) *Daemon {
	d := &Daemon{locker: locker}
	d.cond.L = &d.mu
	d.wake = make(chan struct{}, 1)
	go d.daemon()
	return d
}

// Stop stops d.
func (d *Daemon) Stop() {
	// NOTE(joshlf): Stop may return before the daemon goroutine
	// has returned, but the goroutine will return eventually.
	// Critically, after Stop has returned, the daemon cannot
	// interact with any memory other than d in any way including
	// executing timeout callbacks and calling methods on d.locker,
	// so the amount of time it takes for the daemon to finally
	// return does not affect the correctness of the rest of
	// the program.
	d.mu.Lock()
	d.stopped = true
	if len(d.timeouts) == 0 {
		// the daemon might be waiting on d.cond
		d.cond.Broadcast()
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
	d.mu.Unlock()
}

// AddTimeout schedules f to be called at time t, which must be calculated
// relative to NowMonotonic (not time.Now). The returned *Timeout can be used
// to cancel the timeout, in which case f will not be called. It is guaranteed
//  that f will not be called before time t.
func (d *Daemon) AddTimeout(f func(), t time.Time) *Timeout {
	to := &Timeout{f: f, t: t}
	d.mu.Lock()
	heap.Push(&d.timeouts, to)
	if len(d.timeouts) == 1 {
		// there were previously 0 which means that
		// the daemon might be waiting on d.cond
		d.cond.Broadcast()
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
	d.mu.Unlock()
	return to
}

func (d *Daemon) daemon() {
	for {
		d.mu.Lock()
		if d.stopped {
			d.mu.Unlock()
			return
		}

		if len(d.timeouts) == 0 {
			// no timeouts; block until one is available
			d.cond.Wait()
			if d.stopped {
				d.mu.Unlock()
				return
			}
		}

		for {
			// loop until we're sure it's after to.t (to keep
			// guarantee documented in d.AddTimeout)

			// Do d.peek() inside the loop in case a client
			// called AddTimeout, woke us up, and the timeout
			// they added is sooner than the previous heap min
			to := d.peek()
			now := NowMonotonic()
			if now.After(to.t) {
				break
			}
			d.mu.Unlock()
			select {
			case <-time.After(to.t.Sub(now)):
			case <-d.wake:
			}
			d.mu.Lock()
			if d.stopped {
				d.mu.Unlock()
				return
			}
		}

		to := heap.Pop(&d.timeouts).(*Timeout)
		if atomic.LoadUint32(&to.cancel) == 0 {
			// it wasn't cancelled; we now have to release d.mu
			// before acquiring d.locker in order to avoid a
			// deadlock with another goroutine calling d.AddTimeout.

			d.mu.Unlock()
			d.locker.Lock()
			d.mu.Lock()
			if d.stopped {
				d.mu.Unlock()
				d.locker.Unlock()
				return
			}

			// The only modifications to t that are allowed by
			// goroutines other than this one are stopping t
			// (which we just checked for) and inserting things
			// into the d.timeouts heap. Something being inserted
			// into the d.timeouts heap doesn't invalidate the
			// current timeout we're working on, so we can ignore
			// it. If any timeouts that were inserted were
			// supposed to fire already, they will be handled
			// in the next loop iteration.

			if atomic.LoadUint32(&to.cancel) == 0 {
				// it wasn't cancelled between checking to.cancel
				// and acquiring d.locker
				to.f()
			}
			d.locker.Unlock()
		}
		d.mu.Unlock()
	}
}

// assumes len(d.timeouts) > 0
func (d *Daemon) peek() *Timeout {
	// From the container/heap documentation:
	//
	// Any type that implements heap.Interface may be used as a
	// min-heap with the following invariants (established after
	// Init has been called or if the data is empty or sorted)...
	//
	// Since a sorted list is a valid heap, it must mean that
	// the smallest element is stored in index 0. Thus, the following
	// is not only safe because of the implementation of the
	// heap package, but is actually safe as long as the package's
	// public documentation holds.
	return d.timeouts[0]
}

type heapTimeouts []*Timeout

func (h *heapTimeouts) Len() int           { return len(*h) }
func (h *heapTimeouts) Less(i, j int) bool { return (*h)[i].t.Before((*h)[j].t) }
func (h *heapTimeouts) Swap(i, j int)      { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }
func (h *heapTimeouts) Push(x interface{}) { *h = append(*h, x.(*Timeout)) }
func (h *heapTimeouts) Pop() interface{} {
	x := (*h)[len(*h)-1]
	*h = (*h)[:len(*h)-1]
	return x
}

// NowMonotonic is like time.Now, but the result is monotonically increasing,
// and does not necessarily correspond to the actual current time.
func NowMonotonic() time.Time {
	now := nanotime()
	return time.Unix(now/1e9, now%1e9)
}

// NOTE(joshlf): runtime.nanotime uses CLOCK_MONOTONIC (see discussion
// starting at https://github.com/golang/go/issues/12914#issuecomment-150579623)

//go:linkname nanotime runtime.nanotime
func nanotime() int64
