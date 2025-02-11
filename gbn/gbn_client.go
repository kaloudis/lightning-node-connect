package gbn

import (
	"context"
	"fmt"
	"io"
	"math"
	"time"
)

// NewClientConn creates a new bidirectional Go-Back-N client.
// The sendStream function must write to the underlying transport stream.
// The receiveStream function must read from an underlying transport stream.
// The resendTimeout parameter defines the duration to wait before resending data
// if the corresponding ACK for the data is not received.
func NewClientConn(n uint8, sendFunc sendBytesFunc, receiveFunc recvBytesFunc,
	opts ...Option) (*GoBackNConn, error) {

	if n == math.MaxUint8 {
		return nil, fmt.Errorf("n must be smaller than %d",
			math.MaxUint8)
	}

	conn := newGoBackNConn(
		context.Background(), sendFunc, receiveFunc, false, n,
	)

	// Apply functional options
	for _, o := range opts {
		o(conn)
	}

	if err := conn.clientHandshake(); err != nil {
		if err := conn.Close(); err != nil {
			log.Errorf("error closing gbn ClientConn: %v", err)
		}
		return nil, err
	}
	conn.start()

	return conn, nil
}

// clientHandshake initiates the client side GBN handshake.
// The handshake sequence from the client side is as follows:
// 1.  The client sends SYN to the server along with the N value that the
//     client wishes to use for the connection.
// 2.  The client then waits for the server to respond with SYN.
// 3a. If the client receives SYN from the server then the client sends back
//     SYNACK.
// 3b. If the client does not receive SYN from the server within a given
//     timeout, then the client restarts the handshake from step 1.
func (g *GoBackNConn) clientHandshake() error {
	// Spin off the recv function in a goroutine so that we can use
	// a select to choose to timeout waiting for data from the receive
	// stream. This is needed instead of a context timeout because the
	// recvFromStream function uses the passed context to set up
	// connections (such as to the hash mail server) and we dont want to
	// cancel the contexts of those connections.
	recvChan := make(chan []byte)
	recvNext := make(chan int, 1)
	errChan := make(chan error, 1)
	handshakeComplete := make(chan struct{})
	defer close(handshakeComplete)

	go func() {
		for {
			// We only move on to read from the stream if
			// the handshake is not yet complete and if we get
			// a signal from the recvNext channel.
			select {
			case <-handshakeComplete:
				return
			case <-g.ctx.Done():
				return
			case <-g.quit:
				return
			case <-recvNext:
			}

			b, err := g.recvFromStream(g.ctx)
			if err != nil {
				errChan <- err
				return
			}

			select {
			case <-handshakeComplete:
				return
			case <-g.ctx.Done():
				return
			case <-g.quit:
				return
			case recvChan <- b:
			}
		}
	}()

	var resp Message
handshake:
	for {
		// start Handshake
		msg := &PacketSYN{N: g.n}
		msgBytes, err := msg.Serialize()
		if err != nil {
			return err
		}

		// Send SYN
		log.Debugf("Client sending SYN")
		if err := g.sendToStream(g.ctx, msgBytes); err != nil {
			return err
		}

		for {
			// Wait for SYN
			log.Debugf("Client waiting for SYN")
			select {
			case recvNext <- 1:
			case <-g.quit:
				return nil
			case <-g.ctx.Done():
				return g.ctx.Err()
			default:
			}

			var b []byte
			select {
			case <-time.After(g.handshakeTimeout):
				log.Debugf("SYN resendTimeout. Resending SYN.")
				continue handshake
			case <-g.quit:
				return nil
			case <-g.ctx.Done():
				return g.ctx.Err()
			case err := <-errChan:
				return err
			case b = <-recvChan:
			}

			resp, err = Deserialize(b)
			if err != nil {
				return err
			}

			log.Debugf("Client got %T", resp)
			switch resp.(type) {
			case *PacketSYN:
				break handshake
			default:
			}

			// If we received something other than SYN, we read
			// again from the receive stream since we might just
			// have read a message from a previous connection.
		}
	}

	log.Debugf("Client got SYN")
	if resp.(*PacketSYN).N != g.n {
		return io.EOF
	}

	// Send SYNACK
	log.Debugf("Client sending SYNACK")
	synack, err := new(PacketSYNACK).Serialize()
	if err != nil {
		return err
	}

	if err := g.sendToStream(g.ctx, synack); err != nil {
		return err
	}

	log.Debugf("Client Handshake complete")

	return nil
}
