package dash

import "testing"

func TestBroadcasterSendsToEveryListener(t *testing.T) {
	var b broadcaster
	first, stopFirst := b.listen()
	second, _ := b.listen()

	b.send([]byte("update"))
	for _, ch := range []<-chan []byte{first, second} {
		if got := string(<-ch); got != "update" {
			t.Fatalf("listener got %q", got)
		}
	}

	stopFirst()
	if _, ok := <-first; ok {
		t.Fatal("stopped listener is still open")
	}
	stopFirst()

	b.close()
	if _, ok := <-second; ok {
		t.Fatal("listener is still open after close")
	}
	if _, ok := <-mustListen(&b); ok {
		t.Fatal("listener after close is open")
	}
	b.send([]byte("ignored"))
}

func TestBroadcasterDoesNotBlockOnSlowListener(t *testing.T) {
	var b broadcaster
	ch, stop := b.listen()
	defer stop()
	for i := 0; i < 100; i++ {
		b.send([]byte("update"))
	}
	if len(ch) != cap(ch) {
		t.Fatalf("listener holds %d of %d messages", len(ch), cap(ch))
	}
}

func mustListen(b *broadcaster) <-chan []byte {
	ch, _ := b.listen()
	return ch
}
