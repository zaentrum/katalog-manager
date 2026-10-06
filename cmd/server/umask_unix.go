//go:build unix

package main

import "syscall"

// setUmask has every folder and file the service creates on the share
// writable by its group, as every writer of the library runs: 0775 and 0664.
func setUmask() { syscall.Umask(0o002) }
