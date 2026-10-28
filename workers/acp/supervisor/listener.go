package supervisor

import (
	"context"
	"net"
)

// ListenTCP keeps supervisor control and proxy sockets on ordinary TCP,
// independent of the platform's default use of Multipath TCP.
func ListenTCP(network, address string) (net.Listener, error) {
	var config net.ListenConfig
	config.SetMultipathTCP(false)
	return config.Listen(context.Background(), network, address)
}
