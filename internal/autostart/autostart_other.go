//go:build !darwin && !windows && !linux

package autostart

import "errors"

func set(bool) error { return errors.New("autostart: unsupported on this platform") }
