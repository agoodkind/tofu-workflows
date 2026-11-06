package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"time"
)

type target struct {
	Name    string
	Address string
}
type result struct {
	Name       string `json:"name"`
	Connected  bool   `json:"connected"`
	DurationMS int64  `json:"duration_ms"`
	Failure    string `json:"failure,omitempty"`
}

func main() {
	targets := []target{
		{"qa-planning-ssh", "[3d06:bad:b01:210::217]:22"},
		{"qa-application-ssh", "[3d06:bad:b01:210::223]:22"},
		{"qa-proxmox-api", "[3d06:bad:b01:200::1]:8006"},
		{"production-proxmox-api", "[3d06:bad:b01::254]:8006"},
		{"r2-endpoint", "ee7d7ca7d611ef8c2a07885e8362de0c.r2.cloudflarestorage.com:443"},
		{"registry-baseline", "ghcr.io:443"},
	}
	encoder := json.NewEncoder(os.Stdout)
	failed := false
	for _, destination := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		started := time.Now()
		connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", destination.Address)
		entry := result{Name: destination.Name, Connected: err == nil, DurationMS: time.Since(started).Milliseconds()}
		if err != nil {
			failed = true
			entry.Failure = "connection-failed"
			var operation *net.OpError
			if errors.As(err, &operation) && operation.Timeout() {
				entry.Failure = "timeout"
			}
		} else {
			connection.Close()
		}
		cancel()
		if err := encoder.Encode(entry); err != nil {
			os.Exit(2)
		}
	}
	if failed {
		os.Exit(1)
	}
}
