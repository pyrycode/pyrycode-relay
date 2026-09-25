package relay

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// phoneOutboxBudget bounds the bytes each phone's outbox may pin — pending
// frames plus the one being written: 4 MiB, the size of 16 max-size (maxFrameBytes =
// 256 KiB) frames, 64 MiB for a full server-id of 16 phones, a quarter of a
// 256 MB machine. It counts bytes, not frames, because the daemon's
// connect-time reconciles send one small frame per session per kind: a
// burst of hundreds read off the binary in a few TCP reads, enqueued before
// run has written more than one or two (#154). See
// docs/specs/architecture/154-phone-outbox-byte-budget.md.
const phoneOutboxBudget = 4 << 20

// outboxItemOverhead is charged per item on top of its frame bytes, so a
// flood of tiny or empty frames is bounded in count too: at most
// phoneOutboxBudget / outboxItemOverhead = 65536 items. It covers the
// item's queue slot and its frame's allocation rounding.
const outboxItemOverhead = 64

var (
	// ErrPhoneBacklogFull is returned by phoneOutbox.Enqueue when the phone's
	// queue is at its bound. The outbox closes the phone (1011) before
	// returning; the frame and everything pending are dropped.
	ErrPhoneBacklogFull = errors.New("relay: phone delivery backlog full")

	// ErrPhoneOutboxClosed is returned by phoneOutbox.Enqueue once the outbox
	// has stopped: the phone disconnected, was closed by a directive, or
	// failed a write or the backlog bound.
	ErrPhoneOutboxClosed = errors.New("relay: phone outbox closed")
)

// outboxItem is one pending delivery. A non-zero closeCode marks a close
// directive, whose frame is optional.
type outboxItem struct {
	frame     []byte
	closeCode uint16
}

// phoneOutbox wraps a phone Conn with a bounded FIFO and a single writer
// (run), so the binary forwarder hands frames off without ever waiting on
// the phone's socket (#113). ClientHandler registers the outbox, not the
// bare *WSConn, and owns run's goroutine.
//
// Enqueue, Send, ConnID, Close, CloseWithCode and stop are safe for
// concurrent use. Close and CloseWithCode act immediately rather than
// queueing: eviction, grace expiry and shutdown must cut the connection
// now, and closing the conn is what aborts a write run has in flight.
type phoneOutbox struct {
	conn      Conn
	serverID  string
	logger    *slog.Logger
	onWritten func()

	// mu guards queue (pending items, FIFO) and queued (bytes charged by
	// itemCost for those items plus the one run is delivering). It is a
	// leaf: nothing is called while holding it.
	mu     sync.Mutex
	queue  []outboxItem
	queued int
	budget int

	// ready wakes run after an Enqueue. Capacity 1 and never closed; done
	// signals stop, so a late Enqueue cannot panic on a closed channel.
	ready    chan struct{}
	done     chan struct{}
	doneOnce sync.Once

	// closeRequested records that the binary sent this phone a close
	// directive, so the handler does not report the close back (#152).
	closeRequested atomic.Bool
}

// newPhoneOutbox wraps conn with a queue that may pin at most budget bytes,
// as charged by itemCost. onWritten, which may be nil, runs after each frame
// actually written to conn.
func newPhoneOutbox(conn Conn, serverID string, budget int, onWritten func(), logger *slog.Logger) *phoneOutbox {
	return &phoneOutbox{
		conn:      conn,
		serverID:  serverID,
		logger:    logger,
		onWritten: onWritten,
		budget:    budget,
		ready:     make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

// itemCost is what one queued item charges against the budget.
func itemCost(frame []byte) int { return len(frame) + outboxItemOverhead }

// ConnID returns the wrapped conn's id.
func (o *phoneOutbox) ConnID() string { return o.conn.ConnID() }

// Send queues msg for delivery; it never blocks. See Enqueue.
func (o *phoneOutbox) Send(msg []byte) error { return o.Enqueue(msg, 0) }

// Close closes the wrapped conn immediately, dropping anything pending.
func (o *phoneOutbox) Close() { o.conn.Close() }

// CloseWithCode closes the wrapped conn immediately with code, keeping the
// gracefulCloser capability the registry's eviction and Shutdown rely on.
func (o *phoneOutbox) CloseWithCode(code websocket.StatusCode, reason string) {
	closeWithCode(o.conn, code, reason)
}

// Enqueue queues frame, and a close directive when closeCode is non-zero,
// behind everything already pending. It never blocks. When the item would
// take the outbox past its byte budget the phone can't keep up: the outbox stops, the conn is closed
// off this goroutine (a close can block on the handshake) with 1011 — or
// with closeCode for an overflowing close directive, the daemon's intent —
// and ErrPhoneBacklogFull is returned.
func (o *phoneOutbox) Enqueue(frame []byte, closeCode uint16) error {
	select {
	case <-o.done:
		return ErrPhoneOutboxClosed
	default:
	}
	if closeCode != 0 {
		o.closeRequested.Store(true)
	}
	cost := itemCost(frame)
	o.mu.Lock()
	if o.queued+cost <= o.budget {
		o.queue = append(o.queue, outboxItem{frame: frame, closeCode: closeCode})
		o.queued += cost
		o.mu.Unlock()
		select {
		case o.ready <- struct{}{}:
		default: // run already has a wake-up pending
		}
		return nil
	}
	o.mu.Unlock()
	code := websocket.StatusInternalError
	if closeCode != 0 {
		code = websocket.StatusCode(closeCode)
	}
	if o.markDone() {
		go closeWithCode(o.conn, code, "phone backlog full")
	}
	return ErrPhoneBacklogFull
}

// run writes queued items to the conn in order until the outbox stops, a
// write fails, or a close directive is delivered. A failed write closes the
// phone with 1011 and drops what is pending. The caller runs it on its own
// goroutine and waits for it to return.
func (o *phoneOutbox) run() {
	for {
		select {
		case <-o.done:
			return
		case <-o.ready:
		}
		for {
			it, ok := o.next()
			if !ok {
				break
			}
			more := o.deliver(it)
			o.mu.Lock()
			o.queued -= itemCost(it.frame)
			o.mu.Unlock()
			if !more {
				return
			}
		}
	}
}

// next pops the oldest pending item. Its cost stays charged until run has
// delivered it, so the budget covers the frame being written too.
func (o *phoneOutbox) next() (outboxItem, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.queue) == 0 {
		return outboxItem{}, false
	}
	it := o.queue[0]
	o.queue[0] = outboxItem{}
	o.queue = o.queue[1:]
	if len(o.queue) == 0 {
		o.queue = nil // let the drained backing array go
	}
	return it, true
}

// deliver writes one item and reports whether run should continue.
func (o *phoneOutbox) deliver(it outboxItem) bool {
	select {
	case <-o.done:
		return false
	default:
	}
	if it.closeCode == 0 || hasFrame(it.frame) {
		if err := o.conn.Send(it.frame); err != nil {
			o.markDone()
			if it.closeCode != 0 {
				o.logger.Info("binary_forwarder_close_frame_send_failed",
					"server_id", o.serverID,
					"conn_id", o.conn.ConnID(),
					"close_code", it.closeCode,
					"err", err)
				closePhone(o.conn, it.closeCode)
				return false
			}
			o.logger.Info("binary_forwarder_phone_send_failed",
				"server_id", o.serverID,
				"conn_id", o.conn.ConnID(),
				"err", err)
			closeWithCode(o.conn, websocket.StatusInternalError, "phone write failed")
			return false
		}
		if o.onWritten != nil {
			o.onWritten()
		}
	}
	if it.closeCode != 0 {
		o.markDone()
		closePhone(o.conn, it.closeCode)
		o.logger.Info("binary_forwarder_phone_closed",
			"server_id", o.serverID,
			"conn_id", o.conn.ConnID(),
			"close_code", it.closeCode)
		return false
	}
	return true
}

// closedByBinary reports whether the binary asked for this phone to be
// closed: Enqueue accepted, or overflowed on, a close directive while the
// outbox was live.
func (o *phoneOutbox) closedByBinary() bool { return o.closeRequested.Load() }

// stop releases run and makes later Enqueue calls fail. Idempotent. It
// does not close the conn: the caller owns that.
func (o *phoneOutbox) stop() { o.markDone() }

// markDone closes done once and reports whether this call closed it.
func (o *phoneOutbox) markDone() bool {
	first := false
	o.doneOnce.Do(func() {
		close(o.done)
		first = true
	})
	return first
}
