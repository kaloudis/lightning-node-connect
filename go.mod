module github.com/lightninglabs/lightning-node-connect

require (
	github.com/btcsuite/btcd/btcec/v2 v2.1.3
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/go-errors/errors v1.0.1
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.5.0
	github.com/kkdai/bstream v1.0.0
	github.com/lightninglabs/aperture v0.1.17-beta.0.20220328072456-4a2632d0be38
	github.com/lightninglabs/lightning-node-connect/hashmailrpc v1.0.2
	github.com/lightningnetwork/lnd v0.14.1-beta.0.20220324135938-0dcaa511a249
	github.com/lightningnetwork/lnd/ticker v1.1.0
	github.com/lightningnetwork/lnd/tor v1.0.0
	github.com/stretchr/testify v1.7.0
	golang.org/x/crypto v0.0.0-20210921155107-089bfa567519
	google.golang.org/grpc v1.39.0
	google.golang.org/protobuf v1.27.1
	nhooyr.io/websocket v1.8.7
)

go 1.16

replace github.com/lightningnetwork/lnd => github.com/guggero/lnd v0.11.0-beta.rc4.0.20220329162351-220e3af55339
