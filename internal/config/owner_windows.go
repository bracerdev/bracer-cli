//go:build windows

package config

import "os"

func fileOwner(os.FileInfo) (int, int, bool) { return 0, 0, false }
