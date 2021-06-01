// Code generated by falafel 0.9.1. DO NOT EDIT.
// source: verrpc/verrpc.proto

// +build js

package main

import (
	"context"

	gateway "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"google.golang.org/grpc"
)

func RegisterVersionerJSONCallbacks(registry map[string]func(ctx context.Context,
	conn *grpc.ClientConn, reqJSON string, callback func(string, error))) {

	marshaler := &gateway.JSONPb{
		OrigName: true,
	}

	registry["verrpc.Versioner.GetVersion"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &verrpc.VersionRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := verrpc.NewVersionerClient(conn)
		resp, err := client.GetVersion(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		respBytes, err := marshaler.Marshal(resp)
		if err != nil {
			callback("", err)
			return
		}
		callback(string(respBytes), nil)
	}
}
