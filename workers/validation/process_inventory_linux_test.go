//go:build linux

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOwnedProcessInventoryChecksOnlyDescendantIdentities(t *testing.T) {
	manager := newProcessManager()
	manager.processes = []*childProcess{{pid: 11, uid: 1000}}
	inventory := map[int]processIdentity{
		10: {pid: 10, parent: 1, uids: [4]int{1000, 1000, 1000, 1000}},
		11: {pid: 11, parent: 10, uids: [4]int{1000, 1000, 1000, 1000}},
		12: {pid: 12, parent: 11, uids: [4]int{1000, 1000, 1000, 1000}},
		20: {pid: 20, parent: 1, uids: [4]int{1000, 0, 0, 0}},
	}
	owned, err := manager.ownedProcessInventory(inventory, 10)
	require.NoError(t, err, "an unrelated setuid process is not owned by this supervisor")
	require.ElementsMatch(t, []processIdentity{inventory[11], inventory[12]}, owned)

	changed := inventory[12]
	changed.uids[1] = 0
	inventory[12] = changed
	_, err = manager.ownedProcessInventory(inventory, 10)
	require.ErrorContains(t, err, "owned process changed its sandbox identity")
}
