package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// The host may run the shutdown export while a plugin goroutine is still
// calling back into it. Run with -race: the host API pointer must be read and
// cleared without a data race.
func TestShutdownRacesHostCall(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := hostCall(pluginabi.MethodHostLog, nil); err == nil {
			t.Error("host call without a host API reported success")
		}
	}()
	cliproxyPluginShutdown()
	<-done
}
