//go:build !unix

package config

import "os"

// fileOwnerUID has no owner to report outside unix; the check is skipped.
func fileOwnerUID(os.FileInfo) (uint32, bool) { return 0, false }

// effectiveUID has no unix user id to compare against.
func effectiveUID() uint32 { return 0 }
