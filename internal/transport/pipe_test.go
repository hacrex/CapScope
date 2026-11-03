package transport

import (
	"net"
	"testing"
	"time"
)

func TestPipeFraming(t *testing.T) {
	srv, cli := net.Pipe()
	sc := WrapConn(srv)
	cc := WrapConn(cli)

	done := make(chan error, 1)
	go func() {
		var hello Hello
		if err := sc.Recv(&hello); err != nil {
			done <- err
			return
		}
		if hello.HostID != "node-1" {
			done <- &errorString{"bad host " + hello.HostID}
			return
		}
		done <- sc.Send(Ack{HostID: hello.HostID, OK: true})
	}()

	if err := cc.Send(Hello{HostID: "node-1", Version: "test"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var ack Ack
	if err := cc.Recv(&ack); err != nil {
		t.Fatalf("recv: %v", err)
	}
	if !ack.OK {
		t.Fatalf("bad ack %+v", ack)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server didn't complete")
	}
}

type errorString struct{ s string }

func (e *errorString) Error() string { return e.s }
