// Package workspacedelta captures and compares bounded workspace trees and
// produces deterministic, publication-safe change artifacts.
//
// Capture and Build never follow workspace symlinks. Callers are responsible
// for freezing the workspace process tree before Build and keeping it frozen
// until the returned artifact is durably stored.
package workspacedelta
