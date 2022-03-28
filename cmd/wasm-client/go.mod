module github.com/lightninglabs/lightning-node-connect/cmd/wasm-client

require (
	github.com/btcsuite/btcd/btcec/v2 v2.1.3
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/jessevdk/go-flags v1.4.0
	github.com/lightninglabs/lightning-node-connect v0.0.0-20210728113920-e9de05e8c4ab
	github.com/lightninglabs/loop v0.17.0-beta.0.20220325153929-deec719dfcfa
	github.com/lightninglabs/pool v0.5.5-alpha.0.20220328132942-a85a4ae60e3f
	github.com/lightningnetwork/lnd v0.14.1-beta.0.20220324135938-0dcaa511a249
	google.golang.org/grpc v1.39.0
)

replace github.com/lightninglabs/lightning-node-connect => ../../

replace github.com/lightninglabs/lightning-node-connect/hashmailrpc => ../../hashmailrpc

replace github.com/lightningnetwork/lnd => github.com/guggero/lnd v0.11.0-beta.rc4.0.20220329162351-220e3af55339

go 1.16
