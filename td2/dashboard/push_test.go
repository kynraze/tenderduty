package dash

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// a status update reaches connected browsers on the next tick
func TestStatusUpdateIsPushed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	updates := make(chan *ChainStatus, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, port, updates, make(chan LogMessage), false, func() HealthStatus { return HealthStatus{} }, nil, nil, nil)
	}()
	defer func() { cancel(); <-done }()
	var conn *websocket.Conn
	for deadline := time.Now().Add(5 * time.Second); conn == nil; {
		conn, _, err = websocket.DefaultDialer.Dial("ws://127.0.0.1:"+port+"/ws", nil)
		if err != nil && time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer conn.Close()
	updates <- &ChainStatus{MsgType: "update", Name: "Chain"}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, body, err := conn.ReadMessage()
	if err != nil || !strings.Contains(string(body), `"msgType":"update"`) || !strings.Contains(string(body), `"name":"Chain"`) {
		t.Fatalf("%v: %s", err, body)
	}
}
