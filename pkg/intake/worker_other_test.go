//go:build !linux

package intake

import "testing"

func saveDataLimit(*testing.T) func() { return func() {} }
