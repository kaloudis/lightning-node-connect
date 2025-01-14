package mailbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestSpake2Mask tests the masking operation for SPAK2 to ensure that ti's
// properly reverseable.
func TestSpake2Mask(t *testing.T) {
	t.Parallel()

	priv, err := btcec.NewPrivateKey(btcec.S256())
	require.NoError(t, err)

	pub := priv.PubKey()

	pass := []byte("top secret")
	passHash := sha256.Sum256(pass)

	maskedPoint := ekeMask(pub, passHash[:])
	require.True(t, !maskedPoint.IsEqual(pub))

	unmaskedPoint := ekeUnmask(maskedPoint, passHash[:])
	require.True(t, unmaskedPoint.IsEqual(pub))
}

// TestXXHandshake tests that a client and server can successfully complete a
// Noise_XX pattern handshake and then use the encrypted connection to exchange
// messages afterwards.
func TestXXHandshake(t *testing.T) {
	// First, generate static keys for each party.
	pk1, err := btcec.NewPrivateKey(btcec.S256())
	require.NoError(t, err)

	pk2, err := btcec.NewPrivateKey(btcec.S256())
	require.NoError(t, err)

	// Create a passphraseEntropy that will be used to mask the first ephemeral key.
	pass := []byte("top secret")
	passHash := sha256.Sum256(pass)

	// The server will be initialised with auth data that it is expected to
	// send to the client during act 2 of the handshake.
	authData := []byte("authData")

	// Create a pipe and give one end to the client and one to the server
	// as the underlying transport.
	conn1, conn2 := newMockProxyConns()
	defer func() {
		conn1.Close()
		conn2.Close()
	}()

	// Create a server.
	serverData := NewConnData(
		&keychain.PrivKeyECDH{PrivKey: pk1}, nil, passHash[:], authData,
		nil, nil,
	)
	server := NewNoiseGrpcConn(serverData)

	// Spin off the server's handshake process.
	var (
		serverConn    net.Conn
		serverErrChan = make(chan error)
	)
	go func() {
		var err error
		serverConn, _, err = server.ServerHandshake(conn1)
		serverErrChan <- err
	}()

	// Create a client.
	clientData := NewConnData(
		&keychain.PrivKeyECDH{PrivKey: pk2}, nil, passHash[:], nil,
		nil, nil,
	)
	client := NewNoiseGrpcConn(clientData)

	// Start the client's handshake process.
	clientConn, _, err := client.ClientHandshake(
		context.Background(), "", conn2,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the server's handshake to complete or timeout.
	select {
	case err := <-serverErrChan:
		if err != nil {
			t.Fatal(err)
		}

	case <-time.After(time.Second):
		t.Fatalf("handshake timeout")
	}

	// Ensure that any auth data was successfully received by the client.
	require.True(t, bytes.Equal(client.connData.AuthData(), authData))

	// Also check that both parties now have the other parties static key.
	require.True(t, client.connData.RemoteKey().IsEqual(pk1.PubKey()))
	require.True(t, server.connData.RemoteKey().IsEqual(pk2.PubKey()))

	// Check that messages can be sent between client and server normally
	// now.
	msg := make(chan []byte, 10)
	quit := make(chan struct{})
	go func() {
		var payload []byte
		for {
			select {
			case payload = <-msg:

			case <-quit:
				return
			}
			_, err := clientConn.Write(payload)
			require.NoError(t, err)
		}
	}()

	for i := 0; i < 10; i++ {
		testMessage := []byte(fmt.Sprintf("test message %d", i))

		msg <- testMessage

		recvBuffer := make([]byte, len(testMessage))
		_, err = serverConn.Read(recvBuffer)
		require.NoError(t, err)
		require.True(t, bytes.Equal(recvBuffer, testMessage))
	}

	close(quit)
}

// TestKKHandshake tests that a client and server Machine can successfully
// complete a Noise_KK pattern handshake.
func TestKKHandshake(t *testing.T) {
	// First, generate static keys for each party.
	pk1, err := btcec.NewPrivateKey(btcec.S256())
	require.NoError(t, err)

	pk2, err := btcec.NewPrivateKey(btcec.S256())
	require.NoError(t, err)

	// Create a passphraseEntropy that will be used to mask the first ephemeral key.
	pass := []byte("top secret")
	passHash := sha256.Sum256(pass)

	// The server will be initialised with auth data that it is expected to
	// send to the client during act 2 of the handshake.
	authData := []byte("authData")

	// Create a pipe and give one end to the client and one to the server
	// as the underlying transport.
	conn1, conn2 := newMockProxyConns()
	defer func() {
		conn1.Close()
		conn2.Close()
	}()

	// First, we'll initialize a new state machine for the server with our
	// static key, remote static key, passphrase, and also the
	// authentication data.
	server, err := NewBrontideMachine(&BrontideMachineConfig{
		Initiator:           false,
		HandshakePattern:    KKPattern,
		MinHandshakeVersion: MinHandshakeVersion,
		MaxHandshakeVersion: MaxHandshakeVersion,
		ConnData: NewConnData(
			&keychain.PrivKeyECDH{PrivKey: pk1}, pk2.PubKey(),
			passHash[:], authData, nil, nil,
		),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Spin off the server's handshake process.
	var serverErrChan = make(chan error)
	go func() {
		err := server.DoHandshake(conn1)
		serverErrChan <- err
	}()

	// Create a client.
	client, err := NewBrontideMachine(&BrontideMachineConfig{
		Initiator:           true,
		HandshakePattern:    KKPattern,
		MinHandshakeVersion: MinHandshakeVersion,
		MaxHandshakeVersion: MaxHandshakeVersion,
		ConnData: NewConnData(
			&keychain.PrivKeyECDH{PrivKey: pk2}, pk1.PubKey(),
			passHash[:], nil, nil, nil,
		),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Start the client's handshake process.
	if err := client.DoHandshake(conn2); err != nil {
		t.Fatal(err)
	}

	// Wait for the server's handshake to complete or timeout.
	select {
	case err := <-serverErrChan:
		if err != nil {
			t.Fatal(err)
		}

	case <-time.After(time.Second):
		t.Fatalf("handshake timeout")
	}

	// Ensure that any auth data was successfully received by the client.
	require.True(t, bytes.Equal(client.receivedPayload, authData))

	// Also check that both parties now have the other parties static key.
	require.True(t, client.remoteStatic.IsEqual(pk1.PubKey()))
	require.True(t, server.remoteStatic.IsEqual(pk2.PubKey()))

	// Check that messages can be sent between client and server normally
	// now.
	msg := make(chan []byte, 10)
	quit := make(chan struct{})
	go func() {
		clientConn := &NoiseGrpcConn{
			noise:     client,
			ProxyConn: conn2,
		}
		var payload []byte
		for {
			select {
			case payload = <-msg:

			case <-quit:
				return
			}
			_, err := clientConn.Write(payload)
			require.NoError(t, err)
		}
	}()

	serverConn := &NoiseGrpcConn{
		noise:     server,
		ProxyConn: conn1,
	}
	for i := 0; i < 10; i++ {
		testMessage := []byte(fmt.Sprintf("test message %d", i))

		msg <- testMessage

		recvBuffer := make([]byte, len(testMessage))
		_, err = serverConn.Read(recvBuffer)
		require.NoError(t, err)
		require.True(t, bytes.Equal(recvBuffer, testMessage))
	}
}

// TestHandshake tests that client and server are able successfully perform
// a handshake.
func TestHandshake(t *testing.T) {
	largeAuthData := make([]byte, 3*1024*1024)
	_, err := rand.Read(largeAuthData)
	require.NoError(t, err)

	type test struct {
		name                 string
		serverMinVersion     byte
		serverMaxVersion     byte
		clientMinVersion     byte
		clientMaxVersion     byte
		expectedFinalVersion byte
		authData             []byte
	}

	tests := []*test{
		{
			name:                 "server v0 and client v0",
			serverMinVersion:     HandshakeVersion0,
			serverMaxVersion:     HandshakeVersion0,
			clientMinVersion:     HandshakeVersion0,
			clientMaxVersion:     HandshakeVersion0,
			expectedFinalVersion: HandshakeVersion0,
			authData:             []byte{0, 1, 2, 3},
		},
		{
			name:                 "server v1 and client v1",
			serverMinVersion:     HandshakeVersion1,
			serverMaxVersion:     HandshakeVersion1,
			clientMinVersion:     HandshakeVersion1,
			clientMaxVersion:     HandshakeVersion1,
			expectedFinalVersion: HandshakeVersion1,
			authData:             largeAuthData,
		},
		{
			name:                 "server v0 and client [v0, v1]",
			serverMinVersion:     HandshakeVersion0,
			serverMaxVersion:     HandshakeVersion0,
			clientMinVersion:     HandshakeVersion0,
			clientMaxVersion:     HandshakeVersion1,
			expectedFinalVersion: HandshakeVersion0,
			authData:             []byte{0, 1, 2, 3},
		},
		{
			name:                 "server v1 and client [v0, v1]",
			serverMinVersion:     HandshakeVersion0,
			serverMaxVersion:     HandshakeVersion1,
			clientMinVersion:     HandshakeVersion0,
			clientMaxVersion:     HandshakeVersion1,
			expectedFinalVersion: HandshakeVersion1,
			authData:             largeAuthData,
		},
		{
			name:                 "server v2 and client [v0, v2]",
			serverMinVersion:     HandshakeVersion0,
			serverMaxVersion:     HandshakeVersion2,
			clientMinVersion:     HandshakeVersion0,
			clientMaxVersion:     HandshakeVersion2,
			expectedFinalVersion: HandshakeVersion2,
			authData:             []byte{0, 1, 2, 3},
		},
	}

	handshakeTest := func(t *testing.T, test *test, server,
		client *NoiseGrpcConn, conn1, conn2 *mockProxyConn) {

		var (
			serverConn net.Conn
		)
		serverErrChan := make(chan error)
		go func() {
			var err error
			serverConn, _, err = server.ServerHandshake(conn1)
			serverErrChan <- err
		}()

		clientConn, _, err := client.ClientHandshake(
			context.Background(), "", conn2,
		)
		require.NoError(t, err)

		select {
		case err := <-serverErrChan:
			if err != nil {
				t.Fatal(err)
			}

		case <-time.After(time.Second):
			t.Fatalf("handshake timeout")
		}

		// Check that the negotiated version on either side
		// is as expected.
		require.Equal(
			t, test.expectedFinalVersion,
			client.noise.version,
		)
		require.Equal(
			t, test.expectedFinalVersion,
			server.noise.version,
		)

		// Ensure that any auth data was successfully received
		// by the client.
		require.True(
			t, bytes.Equal(
				client.connData.AuthData(), test.authData,
			),
		)

		if test.expectedFinalVersion >= 2 {
			require.True(
				t, client.connData.RemoteKey().IsEqual(
					server.connData.LocalKey().PubKey(),
				),
			)

			require.True(
				t, server.connData.RemoteKey().IsEqual(
					client.connData.LocalKey().PubKey(),
				),
			)
		}

		// Check that messages can be sent between client and
		// server normally now.
		testMessage := []byte("test message")
		go func() {
			_, err := clientConn.Write(testMessage)
			require.NoError(t, err)
		}()

		recvBuffer := make([]byte, len(testMessage))
		_, err = serverConn.Read(recvBuffer)
		require.NoError(t, err)
		require.True(t, bytes.Equal(recvBuffer, testMessage))
	}

	pk1, err := btcec.NewPrivateKey(btcec.S256())
	require.NoError(t, err)

	pk2, err := btcec.NewPrivateKey(btcec.S256())
	require.NoError(t, err)

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			conn1, conn2 := newMockProxyConns()
			defer func() {
				conn1.Close()
				conn2.Close()
			}()

			// First perform the tests without passing in the
			// remote's pub key.
			server := NewNoiseGrpcConn(
				NewConnData(
					&keychain.PrivKeyECDH{PrivKey: pk1},
					nil, pass, test.authData, nil, nil,
				),
				WithMinHandshakeVersion(test.serverMinVersion),
				WithMaxHandshakeVersion(test.serverMaxVersion),
			)

			client := NewNoiseGrpcConn(
				NewConnData(
					&keychain.PrivKeyECDH{PrivKey: pk2},
					nil, pass, nil, nil, nil,
				),
				WithMinHandshakeVersion(test.clientMinVersion),
				WithMaxHandshakeVersion(test.clientMaxVersion),
			)

			handshakeTest(t, test, server, client, conn1, conn2)

			if test.serverMaxVersion < HandshakeVersion2 &&
				test.clientMaxVersion < HandshakeVersion2 {
				return
			}

			// If either the client or server supports a version
			// above or equal to version 2, we do the test again
			// but this time the NoiseGrpcConn's will be initialised
			// with the remote parties static key in order to force
			// the KK pattern handshake.
			server = NewNoiseGrpcConn(
				NewConnData(
					&keychain.PrivKeyECDH{PrivKey: pk1},
					pk2.PubKey(), pass, test.authData, nil,
					nil,
				),
				WithMinHandshakeVersion(test.serverMinVersion),
				WithMaxHandshakeVersion(test.serverMaxVersion),
			)

			// Ensure that any auth data was successfully received
			// by the client.
			require.True(
				t, bytes.Equal(
					client.connData.AuthData(),
					test.authData,
				),
			)

			client = NewNoiseGrpcConn(
				NewConnData(
					&keychain.PrivKeyECDH{PrivKey: pk2},
					pk1.PubKey(), pass, nil, nil, nil,
				),
				WithMinHandshakeVersion(test.clientMinVersion),
				WithMaxHandshakeVersion(test.clientMaxVersion),
			)

			handshakeTest(t, test, server, client, conn1, conn2)
		})
	}
}

var _ ProxyConn = (*mockProxyConn)(nil)

type mockProxyConn struct {
	net.Conn
}

func (m *mockProxyConn) SetRecvTimeout(_ time.Duration) {}

func (m *mockProxyConn) SetSendTimeout(_ time.Duration) {}

func (m *mockProxyConn) ReceiveControlMsg(_ ControlMsg) error {
	return nil
}

func (m *mockProxyConn) SendControlMsg(_ ControlMsg) error {
	return nil
}

func newMockProxyConns() (*mockProxyConn, *mockProxyConn) {
	c1, c2 := net.Pipe()
	return &mockProxyConn{c1}, &mockProxyConn{c2}
}
