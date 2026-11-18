//go:build !linux

package app

func stopTaskHarnessDaemons(string) error { return nil }
