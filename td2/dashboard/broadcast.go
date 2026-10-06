package dash

import "sync"

// broadcaster drops messages for clients that cannot keep up.
type broadcaster struct {
	mux       sync.Mutex
	listeners map[chan []byte]struct{}
	closed    bool
}

func (b *broadcaster) send(msg []byte) {
	b.mux.Lock()
	defer b.mux.Unlock()
	for l := range b.listeners {
		select {
		case l <- msg:
		default:
		}
	}
}

// listen's channel closes with the broadcaster.
func (b *broadcaster) listen() (<-chan []byte, func()) {
	b.mux.Lock()
	defer b.mux.Unlock()
	ch := make(chan []byte, 16)
	if b.closed {
		close(ch)
		return ch, func() {}
	}
	if b.listeners == nil {
		b.listeners = make(map[chan []byte]struct{})
	}
	b.listeners[ch] = struct{}{}
	return ch, func() {
		b.mux.Lock()
		defer b.mux.Unlock()
		if _, ok := b.listeners[ch]; ok {
			delete(b.listeners, ch)
			close(ch)
		}
	}
}

func (b *broadcaster) close() {
	b.mux.Lock()
	defer b.mux.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for l := range b.listeners {
		close(l)
	}
	b.listeners = nil
}
