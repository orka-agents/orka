/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import "context"

func secureRemoveAll(requested string) error {
	return secureRemoveAllContext(context.Background(), requested)
}

func secureResetDirectory(requested string, setOwner bool, uid, gid uint32, protected []string) error {
	return secureResetDirectoryContext(context.Background(), requested, setOwner, uid, gid, protected)
}
