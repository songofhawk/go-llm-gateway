package main

import "testing"

func TestDebugAddressOnlyLoopback(t *testing.T) {
	for _, addr := range []string{"localhost:6060", "127.0.0.1:6060", "[::1]:6060"} {
		if err := validateDebugAddress(addr); err != nil {
			t.Error(addr, err)
		}
	}
	for _, addr := range []string{":6060", "0.0.0.0:6060", "example.com:6060", "[::]:6060", "invalid"} {
		if validateDebugAddress(addr) == nil {
			t.Error("accepted", addr)
		}
	}
	close, done, err := startDiagnostics("", nil)
	if err != nil || done != nil || close == nil {
		t.Fatal("disabled profiler started")
	}
	close()
}
