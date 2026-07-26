//go:build !linux

package vm

func widenPipe(any) error { return nil }
