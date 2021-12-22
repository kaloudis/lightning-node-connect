package mailbox

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lightninglabs/lightning-node-connect/gbn"
	"github.com/lightninglabs/lightning-node-connect/hashmailrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// GrpcClientConn is a type that establishes a base transport connection to a
// mailbox server using a gRPC connection. This type can be used to initiate a
// mailbox transport connection from a native golang environment.
type GrpcClientConn struct {
	*connKit

	hashMailConn hashmailrpc.HashMailClient

	receiveStream   hashmailrpc.HashMail_RecvStreamClient
	receiveStreamMu sync.Mutex

	sendStream   hashmailrpc.HashMail_SendStreamClient
	sendStreamMu sync.Mutex

	gbnConn *gbn.GoBackNConn

	closeOnce sync.Once

	quit chan struct{}
}

// NewGrpcClientConn creates a new client connection with the given receive and
// send session identifiers. The context given as the first parameter will be
// used throughout the connection lifetime.
func NewGrpcClientConn(ctx context.Context, receiveSID,
	sendSID [64]byte) *GrpcClientConn {

	log.Debugf("New client conn, read_stream=%x, write_stream=%x",
		receiveSID[:], sendSID[:])

	c := &GrpcClientConn{
		quit: make(chan struct{}),
	}
	c.connKit = &connKit{
		ctx:        ctx,
		impl:       c,
		receiveSID: receiveSID,
		sendSID:    sendSID,
	}
	return c
}

// recvFromStream is used to receive a payload from the receive socket.
// The function is passed to and used by the gbn connection.
// It therefore takes in and reacts on the cancellation of a context so that
// the gbn connection is able to close independently of the ClientConn.
func (c *GrpcClientConn) recvFromStream(ctx context.Context) ([]byte, error) {
	c.receiveStreamMu.Lock()
	if c.receiveStream == nil {
		c.createReceiveMailBox(ctx, 0)
	}
	c.receiveStreamMu.Unlock()

	for {
		select {
		case <-c.quit:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		c.receiveStreamMu.Lock()
		msg, err := c.receiveStream.Recv()
		if err != nil {
			log.Debugf("Client: got failure on receive socket, "+
				"re-trying: %v", err)

			c.createReceiveMailBox(ctx, retryWait)
			c.receiveStreamMu.Unlock()
			continue
		}
		c.receiveStreamMu.Unlock()

		return msg.Msg, nil
	}
}

// sendToStream is used to send a payload on the send socket. The function
// is passed to and used by the gbn connection. It therefore takes in and
// reacts on the cancellation of a context so that the gbn connection is able to
// close independently of the ClientConn.
func (c *GrpcClientConn) sendToStream(ctx context.Context, payload []byte) error {
	// Set up the send socket if it has not yet been initialized.
	c.sendStreamMu.Lock()
	if c.sendStream == nil {
		c.createSendMailBox(ctx, 0)
	}
	c.sendStreamMu.Unlock()

	// Retry sending the payload to the hashmail server until it succeeds.
	for {
		select {
		case <-c.quit:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		sendInit := &hashmailrpc.CipherBox{
			Desc: &hashmailrpc.CipherBoxDesc{
				StreamId: c.sendSID[:],
			},
			Msg: payload,
		}

		c.sendStreamMu.Lock()
		err := c.sendStream.Send(sendInit)
		if err != nil {
			log.Debugf("Client: got failure on send socket, "+
				"re-trying: %v", err)

			c.createSendMailBox(ctx, retryWait)
			c.sendStreamMu.Unlock()
			continue
		}
		c.sendStreamMu.Unlock()

		return nil
	}
}

// createReceiveMailBox attempts to connect to the hashmail server and
// initialize a read stream for the given mailbox ID. It retries if any errors
// occur.
// TODO(elle): maybe have a max number of retries and close the connection if
// that maximum is exceeded.
func (c *GrpcClientConn) createReceiveMailBox(ctx context.Context,
	initialBackoff time.Duration) {

	waiter := gbn.NewBackoffWaiter(initialBackoff, retryWait, retryWait)

	for {
		select {
		case <-c.quit:
			return
		case <-ctx.Done():
			return
		default:
		}

		waiter.Wait()

		dialOpts := []grpc.DialOption{
			grpc.WithTransportCredentials(credentials.NewTLS(
				&tls.Config{InsecureSkipVerify: true},
			)),
		}
		clientConn, err := grpc.Dial(c.serverAddr, dialOpts...)
		if err != nil {
			log.Debugf("Client: error creating receive client %v",
				err)

			continue
		}
		hmClient := hashmailrpc.NewHashMailClient(clientConn)

		receiveInit := &hashmailrpc.CipherBoxDesc{
			StreamId: c.receiveSID[:],
		}
		c.receiveStream, err = hmClient.RecvStream(ctx, receiveInit)
		if err != nil {
			log.Debugf("Client: error creating receive stream %v",
				err)

			continue
		}

		log.Debugf("Client: receive mailbox initialized")
		return
	}
}

// createSendMailBox attempts to open a websocket to the hashmail server that
// will be used to send packets on.
func (c *GrpcClientConn) createSendMailBox(ctx context.Context,
	initialBackoff time.Duration) {

	waiter := gbn.NewBackoffWaiter(initialBackoff, retryWait, retryWait)

	for {
		select {
		case <-c.quit:
			return
		case <-ctx.Done():
			return
		default:
		}

		waiter.Wait()

		dialOpts := []grpc.DialOption{
			grpc.WithTransportCredentials(credentials.NewTLS(
				&tls.Config{InsecureSkipVerify: true},
			)),
		}
		clientConn, err := grpc.Dial(c.serverAddr, dialOpts...)
		if err != nil {
			log.Debugf("Client: error creating send client %v", err)
			continue
		}

		hmClient := hashmailrpc.NewHashMailClient(clientConn)
		c.sendStream, err = hmClient.SendStream(ctx)
		if err != nil {
			log.Debugf("Client: error creating send stream %v", err)

			continue
		}

		log.Debugf("Client: Send stream created")
		return
	}
}

// Dial returns a net.Conn abstraction over the mailbox connection.
func (c *GrpcClientConn) Dial(_ context.Context, serverHost string) (net.Conn,
	error) {

	c.connKit.serverAddr = serverHost
	c.quit = make(chan struct{})

	gbnConn, err := gbn.NewClientConn(
		gbnN, c.sendToStream, c.recvFromStream,
		gbn.WithTimeout(gbnTimeout),
		gbn.WithHandshakeTimeout(gbnHandshakeTimeout),
		gbn.WithKeepalivePing(gbnClientPingTimeout),
	)
	if err != nil {
		return nil, err
	}
	c.gbnConn = gbnConn

	return c, nil
}

// ReceiveControlMsg tries to receive a control message over the underlying
// mailbox connection.
//
// NOTE: This is part of the Conn interface.
func (c *GrpcClientConn) ReceiveControlMsg(receive ControlMsg) error {
	msg, err := c.gbnConn.Recv()
	if err != nil {
		return fmt.Errorf("error receiving from go-back-n "+
			"connection: %v", err)
	}

	return receive.Deserialize(msg)
}

// SendControlMsg tries to send a control message over the underlying mailbox
// connection.
//
// NOTE: This is part of the Conn interface.
func (c *GrpcClientConn) SendControlMsg(controlMsg ControlMsg) error {
	payloadBytes, err := controlMsg.Serialize()
	if err != nil {
		return err
	}
	return c.gbnConn.Send(payloadBytes)
}

// Close closes the underlying mailbox connection.
//
// NOTE: This is part of the net.Conn interface.
func (c *GrpcClientConn) Close() error {
	var returnErr error
	c.closeOnce.Do(func() {
		log.Debugf("Closing client connection")

		if err := c.gbnConn.Close(); err != nil {
			log.Debugf("Error closing gbn connection: %v", err)
		}

		close(c.quit)

		if c.receiveStream != nil {
			log.Debugf("sending bye on receive socket")
			returnErr = c.receiveStream.CloseSend()
		}

		if c.sendStream != nil {
			log.Debugf("sending bye on send socket")
			returnErr = c.sendStream.CloseSend()
		}
	})

	return returnErr
}

var _ ProxyConn = (*GrpcClientConn)(nil)
