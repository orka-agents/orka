/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

var deniedAddressPrefixes = mustPrefixes(
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"::/128",
	"::1/128",
	"64:ff9b::/96",
	"64:ff9b:1::/48",
	"100::/64",
	"2001::/32",
	"2001:db8::/32",
	"2002::/16",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
)

func mustPrefixes(values ...string) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		result = append(result, netip.MustParsePrefix(value))
	}
	return result
}

func (p *scmEgressProxy) dialAllowedHost(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := splitTarget(address)
	if err != nil {
		return nil, err
	}
	if !p.hostAllowed(host) {
		return nil, errHostDenied
	}
	resolutionContext, cancel := context.WithTimeout(ctx, p.config.ResolutionTimeout)
	addresses, lookupErr := p.config.Resolver.LookupNetIP(resolutionContext, "ip", host)
	cancel()
	if lookupErr != nil || len(addresses) == 0 {
		return nil, errResolutionFailed
	}
	addresses = sortedAddresses(addresses)
	if len(addresses) > maxResolvedAddresses {
		return nil, errResolutionFailed
	}
	if err := validateResolvedAddresses(addresses); err != nil {
		return nil, err
	}
	connectionContext, cancel := context.WithTimeout(ctx, p.config.ConnectTimeout)
	defer cancel()
	return p.connectResolved(connectionContext, network, port, addresses)
}

func splitTarget(address string) (string, string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" {
		return "", "", errHostDenied
	}
	if err := validateHostname(host); err != nil {
		return "", "", errHostDenied
	}
	return host, port, nil
}

func validateResolvedAddresses(addresses []netip.Addr) error {
	for _, address := range addresses {
		if !publicAddress(address) {
			return errAddressDenied
		}
	}
	return nil
}

func publicAddress(address netip.Addr) bool {
	if !address.IsValid() {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() ||
		address.IsUnspecified() {
		return false
	}
	for _, prefix := range deniedAddressPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func (p *scmEgressProxy) connectResolved(
	ctx context.Context,
	network string,
	port string,
	addresses []netip.Addr,
) (net.Conn, error) {
	var failures []error
	for _, address := range addresses {
		connection, err := p.config.Dialer.DialContext(
			ctx,
			network,
			net.JoinHostPort(address.String(), port),
		)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if err := validateConnectedPeer(connection, address); err != nil {
			_ = connection.Close()
			return nil, err
		}
		return connection, nil
	}
	return nil, fmt.Errorf("connect to allowed target: %w", errors.Join(failures...))
}

func validateConnectedPeer(connection net.Conn, expected netip.Addr) error {
	host, _, err := net.SplitHostPort(connection.RemoteAddr().String())
	if err != nil {
		return errAddressDenied
	}
	actual, err := netip.ParseAddr(host)
	if err != nil {
		return errAddressDenied
	}
	actual = actual.Unmap()
	if !publicAddress(actual) || actual != expected.Unmap() {
		return errAddressDenied
	}
	return nil
}

func targetAddress(host string) string {
	return net.JoinHostPort(host, strconv.Itoa(443))
}
