package itest

import (
	"crypto/tls"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/lightning-node-connect/itest/mockrpc"
	"github.com/lightninglabs/lightning-node-connect/mailbox"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type serverHarness struct {
	serverHost string
	insecure   bool
	mockServer *grpc.Server
	server     *mockrpc.Server
	password   [mailbox.NumPasswordWords]string

	errChan chan error

	wg sync.WaitGroup
}

func newServerHarness(serverHost string, insecure bool) *serverHarness {
	return &serverHarness{
		serverHost: serverHost,
		insecure:   insecure,
		errChan:    make(chan error, 1),
	}
}

func (s *serverHarness) stop() {
	s.mockServer.Stop()
	s.wg.Wait()
}

func (s *serverHarness) start(newPassword bool) error {
	if newPassword {
		password, _, err := mailbox.NewPassword()
		if err != nil {
			return err
		}
		s.password = password
	}

	privKey, err := btcec.NewPrivateKey()
	if err != nil {
		return err
	}

	tlsConfig := &tls.Config{}
	if s.insecure {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	pswdEntropy := mailbox.PasswordMnemonicToEntropy(s.password)
	mailboxServer, err := mailbox.NewServer(
		s.serverHost, pswdEntropy[:], grpc.WithTransportCredentials(
			credentials.NewTLS(tlsConfig),
		),
	)
	if err != nil {
		return err
	}

	ecdh := &keychain.PrivKeyECDH{PrivKey: privKey}
	noiseConn := mailbox.NewNoiseGrpcConn(ecdh, nil, pswdEntropy[:])

	s.mockServer = grpc.NewServer(
		grpc.Creds(noiseConn),
		grpc.MaxRecvMsgSize(1024*1024*200),
	)
	s.server = &mockrpc.Server{}

	mockrpc.RegisterMockServiceServer(s.mockServer, s.server)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.errChan <- s.mockServer.Serve(mailboxServer)
	}()

	return nil
}
